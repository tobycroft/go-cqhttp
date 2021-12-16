package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	config2 "github.com/Mrs4s/go-cqhttp/config"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mrs4s/MiraiGo/utils"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/Mrs4s/go-cqhttp/coolq"
	"github.com/Mrs4s/go-cqhttp/global"
	"github.com/Mrs4s/go-cqhttp/internal/param"
	"github.com/Mrs4s/go-cqhttp/modules/api"
	"github.com/Mrs4s/go-cqhttp/modules/config"
	"github.com/Mrs4s/go-cqhttp/modules/filter"
)

// HTTPServer HTTP通信相关配置
type HTTPServer struct {
	Disabled    bool   `yaml:"disabled"`
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Timeout     int32  `yaml:"timeout"`
	LongPolling struct {
		Enabled      bool `yaml:"enabled"`
		MaxQueueSize int  `yaml:"max-queue-size"`
	} `yaml:"long-polling"`
	Post []struct {
		URL    string `yaml:"url"`
		Secret string `yaml:"secret"`
	}

	MiddleWares `yaml:"middlewares"`
}

type httpServer struct {
	HTTP        *http.Server
	api         *api.Caller
	accessToken string
}

// HTTPClient 反向HTTP上报客户端
type HTTPClient struct {
	bot     *coolq.CQBot
	secret  string
	addr    string
	filter  string
	apiPort int
	timeout int32
}

type httpCtx struct {
	json     gjson.Result
	query    url.Values
	postForm url.Values
}

const httpDefault = `  # HTTP 通信设置
  - http:
      # 服务端监听地址
      host: 127.0.0.1
      # 服务端监听端口
      port: 5700
      # 反向HTTP超时时间, 单位秒
      # 最小值为5，小于5将会忽略本项设置
      timeout: 5
      # 长轮询拓展
      long-polling:
        # 是否开启
        enabled: false
        # 消息队列大小，0 表示不限制队列大小，谨慎使用
        max-queue-size: 2000
      middlewares:
        <<: *default # 引用默认中间件
      # 反向HTTP POST地址列表
      post:
      #- url: '' # 地址
      #  secret: ''           # 密钥
      #- url: http://127.0.0.1:5701/ # 地址
      #  secret: ''          # 密钥
`

func init() {
	config.AddServer(&config.Server{
		Brief:   "HTTP通信",
		Default: httpDefault,
		ParseEnv: func() (string, *yaml.Node) {
			if os.Getenv("GCQ_HTTP_PORT") != "" {
				// type convert tools
				toInt64 := func(str string) int64 {
					i, _ := strconv.ParseInt(str, 10, 64)
					return i
				}
				accessTokenEnv := os.Getenv("GCQ_ACCESS_TOKEN")
				node := &yaml.Node{}
				httpConf := &HTTPServer{
					Host: "0.0.0.0",
					Port: 5700,
					MiddleWares: MiddleWares{
						AccessToken: accessTokenEnv,
					},
				}
				param.SetExcludeDefault(&httpConf.Disabled, param.EnsureBool(os.Getenv("GCQ_HTTP_DISABLE"), false), false)
				param.SetExcludeDefault(&httpConf.Host, os.Getenv("GCQ_HTTP_HOST"), "")
				param.SetExcludeDefault(&httpConf.Port, int(toInt64(os.Getenv("GCQ_HTTP_PORT"))), 0)
				if os.Getenv("GCQ_HTTP_POST_URL") != "" {
					httpConf.Post = append(httpConf.Post, struct {
						URL    string `yaml:"url"`
						Secret string `yaml:"secret"`
					}{os.Getenv("GCQ_HTTP_POST_URL"), os.Getenv("GCQ_HTTP_POST_SECRET")})
				}
				_ = node.Encode(httpConf)
				return "http", node
			}
			return "", nil
		},
	})
}

func (h *httpCtx) Get(s string) gjson.Result {
	j := h.json.Get(s)
	if j.Exists() {
		return j
	}
	validJSONParam := func(p string) bool {
		return (strings.HasPrefix(p, "{") || strings.HasPrefix(p, "[")) && gjson.Valid(p)
	}
	if h.postForm != nil {
		if form := h.postForm.Get(s); form != "" {
			if validJSONParam(form) {
				return gjson.Result{Type: gjson.JSON, Raw: form}
			}
			return gjson.Result{Type: gjson.String, Str: form}
		}
	}
	if h.query != nil {
		if query := h.query.Get(s); query != "" {
			if validJSONParam(query) {
				return gjson.Result{Type: gjson.JSON, Raw: query}
			}
			return gjson.Result{Type: gjson.String, Str: query}
		}
	}
	return gjson.Result{}
}

func (s *httpServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var ctx httpCtx
	contentType := request.Header.Get("Content-Type")
	switch request.Method {
	case http.MethodPost:
		if strings.Contains(contentType, "application/json") {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				log.Warnf("获取请求 %v 的Body时出现错误: %v", request.RequestURI, err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if !gjson.ValidBytes(body) {
				log.Warnf("已拒绝客户端 %v 的请求: 非法Json", request.RemoteAddr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			ctx.json = gjson.Parse(utils.B2S(body))
		}
		if strings.Contains(contentType, "application/x-www-form-urlencoded") {
			err := request.ParseForm()
			if err != nil {
				log.Warnf("已拒绝客户端 %v 的请求: %v", request.RemoteAddr, err)
				writer.WriteHeader(http.StatusBadRequest)
			}
			ctx.postForm = request.PostForm
		}
		fallthrough
	case http.MethodGet:
		ctx.query = request.URL.Query()

	default:
		log.Warnf("已拒绝客户端 %v 的请求: 方法错误", request.RemoteAddr)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if status := checkAuth(request, s.accessToken); status != http.StatusOK {
		writer.WriteHeader(status)
		return
	}

	var response global.MSG
	if request.URL.Path == "/" {
		action := strings.TrimSuffix(ctx.Get("action").Str, "_async")
		log.Debugf("HTTPServer接收到API调用: %v", action)
		response = s.api.Call(action, ctx.Get("params"))
	} else {
		action := strings.TrimPrefix(request.URL.Path, "/")
		action = strings.TrimSuffix(action, "_async")
		log.Debugf("HTTPServer接收到API调用: %v", action)
		response = s.api.Call(action, &ctx)
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func checkAuth(req *http.Request, token string) int {
	if token == "" { // quick path
		return http.StatusOK
	}

	auth := req.Header.Get("Authorization")
	if auth == "" {
		auth = req.URL.Query().Get("access_token")
	} else {
		authN := strings.SplitN(auth, " ", 2)
		if len(authN) == 2 {
			auth = authN[1]
		}
	}

	switch auth {
	case token:
		return http.StatusOK
	case "":
		return http.StatusUnauthorized
	default:
		return http.StatusForbidden
	}
}

// runHTTP 启动HTTP服务器与HTTP上报客户端
func runHTTP(bot *coolq.CQBot, node yaml.Node) {
	var conf HTTPServer
	switch err := node.Decode(&conf); {
	case err != nil:
		log.Warn("读取http配置失败 :", err)
		fallthrough
	case conf.Disabled:
		return
	}

	var addr string
	s := &httpServer{accessToken: conf.AccessToken}
	if conf.Host == "" || conf.Port == 0 {
		goto client
	}
	addr = fmt.Sprintf("%s:%d", conf.Host, conf.Port)
	s.api = api.NewCaller(bot)
	if conf.RateLimit.Enabled {
		s.api.Use(rateLimit(conf.RateLimit.Frequency, conf.RateLimit.Bucket))
	}
	if conf.LongPolling.Enabled {
		s.api.Use(longPolling(bot, conf.LongPolling.MaxQueueSize))
	}

	go func() {
		log.Infof("CQ HTTP 服务器已启动: %v", addr)
		s.HTTP = &http.Server{
			Addr:    addr,
			Handler: s,
		}
		if err := s.HTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error(err)
			log.Infof("HTTP 服务启动失败, 请检查端口是否被占用.")
			log.Warnf("将在五秒后退出.")
			time.Sleep(time.Second * 5)
			os.Exit(1)
		}
	}()
client:
	for _, c := range conf.Post {
		if c.URL != "" {
			go HTTPClient{
				bot:     bot,
				secret:  config2.Secret,
				addr:    config2.Remote_address,
				apiPort: config2.Remote_port,
				filter:  conf.Filter,
				timeout: conf.Timeout,
			}.Run()
		}
	}
}

// Run 运行反向HTTP服务
func (c HTTPClient) Run() {
	filter.Add(c.filter)
	if c.timeout < 5 {
		c.timeout = 5
	}
	c.bot.OnEventPush(c.onBotPushEvent)
	log.Infof("HTTP POST上报器已启动: %v", c.addr)
}

func (c *HTTPClient) onBotPushEvent(e *coolq.Event) {
	if c.filter != "" {
		flt := filter.Find(c.filter)
		if flt != nil && !flt.Eval(gjson.Parse(e.JSONString())) {
			log.Debugf("上报Event %v 到 HTTP 服务器 %s 时被过滤.", c.addr, e.JSONBytes())
			return
		}
	}

	client := http.Client{Timeout: time.Second * time.Duration(c.timeout)}
	header := make(http.Header)
	header.Set("X-Self-ID", strconv.FormatInt(c.bot.Client.Uin, 10))
	header.Set("User-Agent", "CQHttp/4.15.0")
	header.Set("Content-Type", "application/json")
	if c.secret != "" {
		mac := hmac.New(sha1.New, []byte(c.secret))
		_, _ = mac.Write(e.JSONBytes())
		header.Set("X-Signature", "sha1="+hex.EncodeToString(mac.Sum(nil)))
	}
	if c.apiPort != 0 {
		header.Set("X-API-Port", strconv.FormatInt(int64(c.apiPort), 10))
	}

	var res *http.Response
	var err error
	const maxAttemptTimes = 5

	for i := 0; i <= maxAttemptTimes; i++ {
		// see https://stackoverflow.com/questions/31337891/net-http-http-contentlength-222-with-body-length-0
		// we should create a new request for every single post trial
		req, err := http.NewRequest("POST", c.addr, bytes.NewReader(e.JSONBytes()))
		if err != nil {
			log.Warnf("上报 Event 数据到 %v 时创建请求失败: %v", c.addr, err)
			return
		}
		req.Header = header

		res, err = client.Do(req)
		if err == nil {
			//goland:noinspection GoDeferInLoop
			defer res.Body.Close()
			break
		}
		if i != maxAttemptTimes {
			log.Warnf("上报 Event 数据到 %v 失败: %v 将进行第 %d 次重试", c.addr, err, i+1)
		}
		const maxWait = int64(time.Second * 3)
		const minWait = int64(time.Millisecond * 500)
		wait := rand.Int63n(maxWait-minWait) + minWait
		time.Sleep(time.Duration(wait))
	}

	if err != nil {
		log.Warnf("上报Event数据 %s 到 %v 失败: %v", e.JSONBytes(), c.addr, err)
		return
	}
	log.Debugf("上报Event数据 %s 到 %v", e.JSONBytes(), c.addr)

	r, err := io.ReadAll(res.Body)
	if err != nil {
		return
	}
	if gjson.ValidBytes(r) {
		c.bot.CQHandleQuickOperation(gjson.Parse(e.JSONString()), gjson.ParseBytes(r))
	}
}

func (s *httpServer) ShutDown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.HTTP.Shutdown(ctx); err != nil {
		log.Fatal("http Server Shutdown:", err)
	}
	<-ctx.Done()
	log.Println("timeout of 5 seconds.")
	log.Println("http Server exiting")
}
