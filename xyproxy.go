package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/elazarl/goproxy"
	"github.com/garyburd/redigo/redis"
	"github.com/gorilla/websocket"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"text/template"
	"time"
)

var hosts map[string]string
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)
var homeTemplate = template.Must(template.ParseFiles("index.html"))

type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
}

type Message struct {
	Method    string
	Code      int
	Host      string
	Path      string
	Mime_type string
	Req       []byte
	Resp      []byte
	// StartTime time.Time
	CostTime time.Duration
}

type UserData struct {
	ReqData []byte
	Time    time.Time
}

func newHub() *Hub {
	return &Hub{
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

func LoadHostsFromRedis(key string, db string) (err error) {
	client, err := redis.Dial("tcp", db)
	if err != nil {
		log.Print("redis connect error")
		return err
	}
	defer client.Close()
	hosts, err = redis.StringMap(client.Do("HGETALL", key))
	if err != nil {
		log.Print("redis hgetall error")
		return err
	} else {
		log.Println("reload hosts: ", hosts)
	}
	return nil
}

func TLSConfigFromCA() func(host string, ctx *goproxy.ProxyCtx) (*tls.Config, error) {
	return func(host string, ctx *goproxy.ProxyCtx) (*tls.Config, error) {
		cert, err := tls.LoadX509KeyPair("ca.crt", "ca.key")
		if err != nil {
			log.Fatalf("Unable to load certificate - %v", err)
		}

		config := &tls.Config{
			InsecureSkipVerify: true,
		}
		config.Certificates = append(config.Certificates, cert)
		return config, nil
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Printf("error: %v", err)
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		c.hub.broadcast <- message
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write(newline)
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

func ServeProxy(name string, addr string, verbose bool, db string) {
	proxy := goproxy.NewProxyHttpServer()

	hub := newHub()
	go hub.run()

	proxy.Tr.Dial = func(network, srcAddr string) (c net.Conn, err error) {
		host, port, err := net.SplitHostPort(srcAddr)
		if destination, ok := hosts[host]; ok {
			destAddr := fmt.Sprintf("%s:%s", destination, port)
			log.Printf("change host from %s to %s", srcAddr, destAddr)
			c, err = net.Dial(network, destAddr)
		} else {
			c, err = net.Dial(network, srcAddr)
		}
		return
	}

	proxy.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/reload" {
			LoadHostsFromRedis(name, db)
			w.WriteHeader(200)
			w.Write([]byte("reload"))
		} else if req.URL.Path == "/ping" {
			w.WriteHeader(200)
			w.Write([]byte("pong"))
		} else if req.URL.Path == "/ws" {
			conn, err := upgrader.Upgrade(w, req, nil)
			if err != nil {
				log.Println(err)
				return
			}
			client := &Client{hub: hub, conn: conn, send: make(chan []byte, 256)}
			client.hub.register <- client
			// log.Println("register client", client)
			go client.writePump()
			client.readPump()
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			homeTemplate.Execute(w, req.Host)
		}
	})

	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		reqContent, _ := httputil.DumpRequestOut(req, true)
		ctx.UserData = &UserData{
			ReqData: reqContent,
			Time:    time.Now(),
		}
		return req, nil
	})

	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		req := ctx.Req
		respContent, _ := httputil.DumpResponse(resp, true)
		reqData := ctx.UserData.(*UserData)
		msg := &Message{
			Method:    req.Method,
			Code:      resp.StatusCode,
			Host:      req.URL.Host,
			Path:      req.URL.Path,
			Mime_type: resp.Header.Get("Content-Type"),
			Req:       reqData.ReqData,
			Resp:      respContent,
			// StartTime: reqData.Time,
			CostTime: time.Now().Sub(reqData.Time),
		}

		msgJson, err := json.Marshal(msg)
		if err != nil {
			log.Println("json err:", err)
		}
		hub.broadcast <- msgJson
		return resp
	})

	proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		if !strings.HasSuffix(host, "simple.com:443") {
			return goproxy.OkConnect, host
		}
		log.Println("connect from", host)

		ip, _, err := net.SplitHostPort(ctx.Req.RemoteAddr)
		if err != nil {
			panic(fmt.Sprintf("userip: %q is not IP:port", ctx.Req.RemoteAddr))
		}
		userIP := net.ParseIP(ip)
		if userIP == nil {
			panic(fmt.Sprintf("userip: %q is not IP", ip))
		}
		log.Printf("Handled connect from ip - %s - for host %s", ip, host)
		if err != nil {
			log.Printf("Error creating URL for host %s", host)
		}

		return &goproxy.ConnectAction{
			Action:    goproxy.ConnectMitm,
			TLSConfig: TLSConfigFromCA(),
		}, host + ":443"
	})
	proxy.Verbose = verbose
	log.Fatal(http.ListenAndServe(addr, proxy))
}

func main() {
	name := flag.String("name", "proxy", "proxy name")
	addr := flag.String("addr", ":8888", "proxy listen address")
	verbose := flag.Bool("v", false, "verbose")
	db := flag.String("db", "127.0.0.1:6379", "redis storage")
	flag.Parse()

	LoadHostsFromRedis(*name, *db)
	log.Println("Serving Proxy on 0.0.0.0 port", *addr)
	ServeProxy(*name, *addr, *verbose, *db)
}
