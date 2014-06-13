package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"

	"github.com/bitly/go-nsq"
	"github.com/garyburd/redigo/redis"
	"github.com/zenazn/goji/web"
)

type ScriptRequest struct {
	ID          int
	Script      string            `json:"script"`
	Args        []string          `json:"args"`
	Files       map[string]string `json:"files"`
	CallbackURL string            `json:"callback_url"`
}

func ServiceRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		body, err := ioutil.ReadAll(r.Body)
		if err == nil {
			log.Printf("Callback: %s\n", body)
		}
	}
	w.Write([]byte("."))
}

func GetAllScripts(w http.ResponseWriter, r *http.Request) {
	// Get a list of all the scripts in script folder.

	log.Println("Received GET", r.URL)

	// Open and parse whitelist
	p := path.Join(config.Worker.ScriptDir, config.Worker.WhiteList)
	file, err := os.Open(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	var buf bytes.Buffer
	buf.WriteString("[")

	scanner := bufio.NewScanner(file)
	scanner.Scan()
	buf.WriteString(scanner.Text())
	for scanner.Scan() {
		buf.WriteString(", ")
		buf.WriteString(scanner.Text())
	}
	buf.WriteString("]")

	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}

func ReloadScripts(w http.ResponseWriter, r *http.Request) {
	// Reload the whitelist of scripts.

	log.Println("Received PUT", r.URL)

	p := path.Join(config.Worker.ScriptDir, config.Worker.WhiteList)

	doneChan := make(chan *nsq.ProducerTransaction)
	err := producer.PublishAsync("reload", []byte(p), doneChan)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-doneChan

	var buf bytes.Buffer
	buf.WriteString("Reload request sent")

	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}

func RunScript(c web.C, w http.ResponseWriter, r *http.Request) {
	// Send details to queue for execution.

	log.Println("Received POST", r.URL)

	// Parse the request
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Println(err)
	}
	var sr ScriptRequest
	err = json.Unmarshal(body, &sr)
	if err != nil {
		log.Println(err)
	}
	id, err := getRedisID()
	if err != nil {
		log.Println(err)
	}
	sr.ID = id
	sr.Script = c.URLParams["name"]

	// Queue up the request
	doneChan := make(chan *nsq.ProducerTransaction)
	data, err := json.Marshal(sr)
	if err != nil {
		log.Println(err)
	}
	err = producer.PublishAsync(config.Topic, data, doneChan)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-doneChan
	log.Println("Request queued as", sr.ID)

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func GetAllLogs(c web.C, w http.ResponseWriter, r *http.Request) {
	// Retrieve all logs for a specific script.

	log.Println("Received GET", r.URL)

	conn := redisDB.Get()
	defer conn.Close()

	// LRANGE returns an array of json strings
	reply, err := redis.Strings(conn.Do("ZRANGE", c.URLParams["name"], 0, -1))
	if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	buf.WriteString("[")

	if len(reply) > 0 {
		buf.WriteString(reply[0])
		for i := 1; i < len(reply); i++ {
			buf.WriteString(", ")
			buf.WriteString(reply[i])
		}
	}
	buf.WriteString("]")
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}

func GetLog(c web.C, w http.ResponseWriter, r *http.Request) {
	// Retrieve a specific log of a specific script.

	log.Println("Received GET", r.URL)

	conn := redisDB.Get()
	defer conn.Close()

	script := c.URLParams["name"]
	id := c.URLParams["id"]

	reply, err := redis.Strings(conn.Do("ZRANGEBYSCORE", script, id, id))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(reply[0]))
}
