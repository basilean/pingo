/*
PinGo

GNU/GPL v3

Pingo produces Prometheus metrics about network between sites or clusters.
It connects to a Kubernetes API with a token and gets a list of cluster nodes.
For each node, it will run a coroutine that probes it.

Usage:

	pingo

Environment variables needed:

	PINGO_API
		API endpoint to connect.
	
	PINGO_TOKEN
		A valid token to authenticate.

Author:

	Andres Basile

*/
package main

import (
	"crypto/tls"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
	"text/template"
	"bytes"
)

// Config is a common place for settings.
type Config struct {
	Api	string
	Token	string
	Probe	int
	Scan	int
}

// Node is basic data about a target host.
type Node struct {
	Name   string
	Target string
}

// Probe is a channel package used from scan to probe.
type Probe struct {
	Node	Node
	Kill	chan struct{}
	Seen	bool
}

// Metric is a channel package used form probe to collect.
type Metric struct {
	Node	Node
	Reply  int
	Lost   int
	Time   int64
}

// Board is a segment of memory shared between collect and publish.
type Board struct {
	bytes.Buffer
	sync.RWMutex
}

// NodeList is used to get json values from api reply.
type NodeList struct {
	Items []struct {
		Status struct {
			Addresses []struct {
				Type    string
				Address string
			}
			DaemonEndpoints struct {
				KubeletEndpoint struct {
					Port int
				}
			}
		}
	}
}

// Scan gets from a Kubernetes api a list of nodes and manages a probe for each one.
func scan(wg *sync.WaitGroup, config *Config, out chan Node, metrics chan Metric) {
	defer wg.Done()
	log.Println("INFO scan init")

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	request, err := http.NewRequest("GET", config.Api + "/api/v1/nodes/", nil)
	if err != nil {
		log.Fatal("ERROR scan creating request:", err)
	}
	request.Header.Add("Authorization", "Bearer " + config.Token)

	metric := Metric{
		Node: Node{
			Name: "API",
			Target: config.Api,
		},
	}

	ticker := time.NewTicker(time.Duration(config.Scan) * time.Second)
	defer ticker.Stop()
	known := make(map[string]*Probe)

	for start := time.Now(); true; start = <- ticker.C {
		reply, err := client.Do(request)
		if err != nil {
			log.Println("ERROR scan performing request:", err)
			metric.Lost++
			metrics <- metric
			continue
		}
		if reply.StatusCode != 200 {
			log.Println("ERROR scan got reply:", reply.StatusCode)
			metric.Lost++
			metrics <- metric
			continue
		}
		metric.Time += time.Since(start).Milliseconds()
		metric.Reply++
		metrics <- metric

		defer reply.Body.Close()
		nodes := NodeList{}
		err = json.NewDecoder(reply.Body).Decode(&nodes)
		if err != nil {
			log.Println("ERROR scan parsing reply body:", err)
			continue
		}

		for _, item := range nodes.Items {
			node := Node{}
			for _, addr := range item.Status.Addresses {
				switch addr.Type {
					case "Hostname":
						node.Name = addr.Address
					case "InternalIP":
						node.Target = addr.Address + ":" + strconv.Itoa(item.Status.DaemonEndpoints.KubeletEndpoint.Port)
					default:
						log.Println("ERROR scan address type:", addr.Type)
				}
			}
			val, exists := known[node.Name]
			if exists && val.Node != node {
				log.Println("INFO scan killing outdated probe:", node.Name)
				known[node.Name].Kill <- struct{}{}
				delete(known, node.Name)
				exists = false
			}
			if !exists {
					known[node.Name] = &Probe{
						Node: node,
						Kill: make(chan struct{}),
					}
					log.Println("INFO scan starting new probe:", node.Name)
					go probe(config.Probe, metrics, known[node.Name])	
			}
			known[node.Name].Seen = true
		}

		for key, _ := range known {
			if known[key].Seen {
				known[key].Seen = false
			} else {
				log.Println("INFO scan killing lost probe:", known[key].Node.Name)
				known[key].Kill <- struct{}{}
				delete(known, key)
			}
		}
	}
}

// Probe is a neverends loop that keeps connecting to node.
func probe(interval int, metrics chan Metric, node *Probe) {
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	timeout := time.Duration(interval - 1) * time.Second
	metric := Metric{
		Node:   node.Node,
	}
	log.Println("INFO probe start:", node.Node.Name)
	for {
		select {
			case <- node.Kill:
				log.Println("INFO probe stop:", node.Node.Name)
				ticker.Stop()
				return
			case start := <-ticker.C:
				// 3-Way Handshake: Syn ->, <- Syn/Ack, Ack ->
				conn, err := net.DialTimeout("tcp4", node.Node.Target, timeout)
				if err != nil {
					metric.Lost++
				} else {
					metric.Time += time.Since(start).Milliseconds()
					metric.Reply++
					// 3-Way Fin: Fin/Ack ->, <- Fin/Ack, Ack ->
					// conn.(*net.TCPConn).SetLinger(0) // Force Rst/Ack instead.
					conn.Close()
				}
				metrics <- metric
		}
	}
}

func collect(wg *sync.WaitGroup, config *Config, in chan Metric, board *Board) {
	defer wg.Done()
	log.Println("INFO collect init")

	const tpl = `# Pingo is RnR
# HELP pingo_reply The total number of replies received.
# TYPE pingo_reply counter
{{- range $key, $val := . }}
pingo_reply{name="{{ $val.Node.Name }}",target="{{ $val.Node.Target }}"} {{ $val.Reply }}
{{- end }}

# HELP pingo_lost The total number of replies NOT received.
# TYPE pingo_lost counter
{{- range $key, $val := . }}
pingo_lost{name="{{ $val.Node.Name }}",target="{{ $val.Node.Target }}"} {{ $val.Lost }}
{{- end }}

# HELP pingo_time The total number of milliseconds spent awaiting replies.
# TYPE pingo_time counter
{{- range $key, $val := . }}
pingo_time{name="{{ $val.Node.Name }}",target="{{ $val.Node.Target }}"} {{ $val.Time }}
{{- end }}`
	t, err := template.New("pingo").Parse(tpl)
	if err != nil {
		log.Fatal("ERROR collect creating template:", err)
	}
	ticker := time.NewTicker(time.Duration(config.Probe) * time.Second)
	known := make(map[string]Metric)
	for {
		select {
			case metric := <- in:
				known[metric.Node.Name] = metric
			case _ = <- ticker.C:
				board.Lock()
				board.Reset()
				err := t.Execute(board, known)
				if err != nil {
					log.Println("ERROR collect executing template:", err)
				}
				board.Unlock()
				// TODO: a better way to handle it.
				api, _ := known["API"]
				known = make(map[string]Metric)
				known["API"] = api
		}
	}
}

func publish(wg *sync.WaitGroup, board *Board) {
	defer wg.Done()
	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request){
		board.RLock()
		w.Write(board.Bytes())
		board.RUnlock()
	})
  err := http.ListenAndServe(":8080", nil)
  if err != nil {
  	log.Fatal("ERROR export creating server:", err)
  }
}

func mustEnv(name string, fatal string) string {
	env := os.Getenv(name)
	if len(env) < 1 {
		log.Fatal(fatal)
	}
	return env
}

func couldEnv(name string, def int) int {
	env, err := strconv.Atoi(os.Getenv(name))
	if err != nil || env < 2 {
		return def
	}
	return env
}

func main() {
	config := Config{
		Api: mustEnv("PINGO_API", "FATAL environment variable PINGO_API not defined."),
		Token: mustEnv("PINGO_TOKEN", "FATAL environment variable PINGO_TOKEN not defined."),
		Scan: couldEnv("PINGO_SCAN", 60),
		Probe: couldEnv("PINGO_PROBE", 15),
	}
	log.Println("INFO config api:", config.Api)

	var wg sync.WaitGroup
	nodes := make(chan Node, 32)
	metrics := make(chan Metric, 32)
	board := Board{}
	go scan(&wg, &config, nodes, metrics)
//	go aim(&wg, &config, nodes, metrics)
	go collect(&wg, &config, metrics, &board)
	go publish(&wg, &board)
	wg.Add(1)
	wg.Wait()
}
