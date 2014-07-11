package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"net/http"

	"strings"
	"syscall"

	"github.com/bitly/go-nsq"
	"github.com/garyburd/redigo/redis"
	"github.com/op/go-logging"
	"github.com/pressly/gohttpware/auth"
	"github.com/pressly/gohttpware/route"
	"github.com/zenazn/goji/graceful"
	"github.com/zenazn/goji/web"
	"github.com/zenazn/goji/web/middleware"
)

var (
	configPath = flag.String("config-file", "./config.toml", "path to qmd config file")
	log        = logging.MustGetLogger("qmd")

	config   Config
	producer *nsq.Producer
	consumer *nsq.Consumer
	redisDB  *redis.Pool
)

const (
	VERSION = "0.1.0"
)

func main() {
	var err error

	flag.Parse()

	// Server config
	err = config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	err = config.Setup()
	if err != nil {
		log.Fatal(err)
	}

	log.Info("=====> [ QMD v%s ] <=====", VERSION)

	// Setup facilities
	producer, err = nsq.NewProducer(config.QueueAddr, nsq.NewConfig())
	if err != nil {
		log.Fatal(err)
	}
	redisDB = newRedisPool(config.RedisAddr)

	// Script processing worker
	worker, err := NewWorker(config)
	if err != nil {
		log.Fatal(err)
	}
	go worker.Run()

	// Http server
	w := web.New()

	w.Use(middleware.Logger)
	w.Use(middleware.Recoverer)
	w.Use(route.Heartbeat)
	if config.Auth.Enabled {
		basicAuthWithRedis := func(r *http.Request, secrets []string) bool {
			var err error
			secret := fmt.Sprintf("%s:%s", secrets[0], secrets[1])

			auth := r.Header.Get("Authorization")
			prefix := "Basic "
			if !strings.HasPrefix(auth, prefix) {
				return false
			}
			givenSecret := auth[len(prefix):]
			decodedSecret, err := base64.StdEncoding.DecodeString(givenSecret)
			if err != nil {
				log.Error(err.Error())
				return false
			}

			user := strings.Split(string(decodedSecret), ":")[0]
			check, err := checkRedisUser(user)
			if err != nil {
				log.Error(err.Error())
				return false
			}
			if check != true {
				log.Error("User: %s is blocked", user)
				return false
			}

			if string(decodedSecret) == secret {
				return true
			}
			if err := failRedisUser(user); err != nil {
				log.Error(err.Error())
			}
			return false
		}

		authFunc := auth.Wrap(
			basicAuthWithRedis,
			"Restricted",
			config.Auth.Username,
			config.Auth.Password,
		)
		w.Use(authFunc)
	}
	w.Use(route.AllowSlash)

	w.Get("/scripts", GetAllScripts)
	w.Put("/scripts", ReloadScripts)
	w.Post("/scripts/:name", RunScript)
	w.Get("/scripts/:name/logs", GetAllLogs)
	w.Get("/scripts/:name/logs/:id", GetLog)
	w.Handle("/*", AdminProxy)

	// Spin up the server with graceful hooks
	graceful.PreHook(func() {
		log.Info("Stopping queue producer")
		producer.Stop()

		log.Info("Stopping queue workers")
		worker.Stop()

		log.Info("Closing redis connections")
		redisDB.Close()
	})

	graceful.AddSignal(syscall.SIGINT, syscall.SIGTERM)

	err = graceful.ListenAndServe(config.ListenOnAddr, w)
	if err != nil {
		log.Fatal(err)
	}
	graceful.Wait()
}
