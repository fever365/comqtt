// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2022 wind
// SPDX-FileContributor: wind (573966@qq.com)

package main

import (
	"encoding/json"
	"flag"
	"github.com/go-redis/redis/v8"
	"github.com/rs/zerolog"
	cs "github.com/wind-c/comqtt/cluster"
	colog "github.com/wind-c/comqtt/cluster/log/zero"
	coredis "github.com/wind-c/comqtt/cluster/storage/redis"
	"github.com/wind-c/comqtt/config"
	mqtt "github.com/wind-c/comqtt/mqtt"
	"github.com/wind-c/comqtt/mqtt/hooks/auth"
	"github.com/wind-c/comqtt/mqtt/listeners"
	"github.com/wind-c/comqtt/plugin"
	hauth "github.com/wind-c/comqtt/plugin/auth/http"
	mauth "github.com/wind-c/comqtt/plugin/auth/mysql"
	pauth "github.com/wind-c/comqtt/plugin/auth/postgresql"
	rauth "github.com/wind-c/comqtt/plugin/auth/redis"
	cokafka "github.com/wind-c/comqtt/plugin/bridge/kafka"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

var server *mqtt.Server
var agent *cs.Agent
var logger *zerolog.Logger

func main() {
	var err error
	var confFile string
	var members string
	cfg := config.New()

	flag.StringVar(&confFile, "conf", "", "read the program parameters from the config file")
	flag.UintVar(&cfg.StorageWay, "storage-way", 3, "storage way options:0 memory, 1 bolt, 2 badger, 3 redis")
	flag.UintVar(&cfg.Auth.Way, "auth-way", 0, "authentication way options:0 anonymous, 1 username and password, 2 clientid")
	flag.UintVar(&cfg.Auth.Datasource, "auth-ds", 0, "authentication datasource options:0 free, 1 redis, 2 mysql, 3 postgresql, 4 http")
	flag.StringVar(&cfg.Auth.ConfPath, "auth-path", "", "config file path should correspond to the auth-datasource")
	flag.StringVar(&cfg.Mqtt.TCP, "tcp", ":1883", "network address for mqtt tcp listener")
	flag.StringVar(&cfg.Mqtt.WS, "ws", ":1882", "network address for mqtt websocket listener")
	flag.StringVar(&cfg.Mqtt.HTTP, "http", ":8080", "network address for web info dashboard listener")
	flag.StringVar(&cfg.Cluster.NodeName, "node-name", "", "node name must be unique in the cluster")
	flag.StringVar(&cfg.Cluster.BindAddr, "bind-ip", "127.0.0.1", "the ip used for discovery and communication between nodes. It is usually set to the intranet ip addr.")
	flag.IntVar(&cfg.Cluster.BindPort, "gossip-port", 7946, "this port is used to discover nodes in a cluster")
	flag.IntVar(&cfg.Cluster.RaftPort, "raft-port", 8946, "this port is used for raft peer communication")
	flag.BoolVar(&cfg.Cluster.RaftBootstrap, "raft-bootstrap", false, "should be `true` for the first node of the cluster. It can elect a leader without any other nodes being present.")
	flag.StringVar(&members, "members", "", "seeds member list of cluster,such as 192.168.0.103:7946,192.168.0.104:7946")
	flag.BoolVar(&cfg.Cluster.GrpcEnable, "grpc-enable", false, "grpc is used for raft transport and reliable communication between nodes")
	flag.IntVar(&cfg.Cluster.GrpcPort, "grpc-port", 17946, "grpc communication port between nodes")
	flag.StringVar(&cfg.Redis.Options.Addr, "redis", "127.0.0.1:6379", "redis address for cluster mode")
	flag.StringVar(&cfg.Redis.Options.Password, "redis-pass", "", "redis password for cluster mode")
	flag.IntVar(&cfg.Redis.Options.DB, "redis-db", 0, "redis db for cluster mode")
	flag.BoolVar(&cfg.Log.Enable, "log-enable", true, "log enabled or not")
	flag.IntVar(&cfg.Log.Env, "env", 0, "app running environment:0 development or 1 production")
	flag.IntVar(&cfg.Log.Level, "level", 1, "log level options:0Debug,1Info, 2Warn, 3Error, 4Fatal, 5Panic, 6NoLevel, 7Off")
	flag.StringVar(&cfg.Log.InfoFile, "info-file", "./logs/co-info.log", "info log filename")
	flag.StringVar(&cfg.Log.ErrorFile, "error-file", "./logs/co-err.log", "error log filename")
	//parse arguments
	flag.Parse()
	//load config file
	if confFile != "" {
		if cfg, err = config.Load(confFile); err != nil {
			log.Fatal(err)
		}
	} else {
		if members != "" {
			cfg.Cluster.Members = strings.Split(members, ",")
		} else {
			cfg.Cluster.Members = []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Cluster.BindPort))}
		}
	}

	//init log
	if cfg.Cluster.NodeName == "" {
		if hn, err := os.Hostname(); err == nil {
			cfg.Log.NodeName = hn
		}
	}

	logger = colog.Init(cfg.Log)
	if cfg.Log.Enable && cfg.Log.Format == 1 {
		log.Println("log output to the files, please check")
	}

	//listen system operations
	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGABRT, syscall.SIGKILL)
	go func() {
		<-sigs
		done <- true
	}()

	// create server instance and init hooks
	cfg.Mqtt.Options.Logger = logger
	server := mqtt.New(&cfg.Mqtt.Options)
	logger.Info().Msg("comqtt server initializing...")
	initStorage(server, cfg)
	initAuth(server, cfg)
	initBridge(server, cfg)

	// init node and bind mqtt server
	if cfg.Cluster.Members == nil {
		onError(config.ErrClusterOpts, "members parameter etc")
	} else {
		initClusterNode(server, cfg)
	}

	// gen tls config
	var listenerConfig *listeners.Config
	if tlsConfig, err := config.GenTlsConfig(cfg); err != nil {
		onError(err, "gen tls config")
	} else {
		if tlsConfig != nil {
			listenerConfig = &listeners.Config{TLSConfig: tlsConfig}
		}
	}

	// add tcp listener
	tcp := listeners.NewTCP("tcp", cfg.Mqtt.TCP, listenerConfig)
	onError(server.AddListener(tcp), "add tcp listener")

	// add websocket listener
	ws := listeners.NewWebsocket("ws", cfg.Mqtt.WS, listenerConfig)
	onError(server.AddListener(ws), "add websocket listener")

	// add http listener
	handles := make(map[string]func(http.ResponseWriter, *http.Request), 1)
	handles["/cluster/conf"] = ConfHandler
	handles["/cluster/ms"] = MsHandler
	handles["/cluster/peer/"] = PeerHandler //for test peer join and leave
	http := listeners.NewHTTP("stats", cfg.Mqtt.HTTP, nil, server.Info, handles)
	onError(server.AddListener(http), "add http listener")

	// start server
	go func() {
		err := server.Serve()
		if err != nil {
			logger.Fatal().Err(err).Send()
		}
	}()

	if cfg.Log.Format == 1 {
		log.Println("comqtt server started")
	}

	// exit
	<-done
	logger.Warn().Msg("caught signal, stopping...")
	agent.Stop()
	server.Close()
	colog.Close()
}

func initAuth(server *mqtt.Server, conf *config.Config) {
	logMsg := "init auth"
	if conf.Auth.Way == config.AuthModeAnonymous {
		server.AddHook(new(auth.AllowHook), nil)
	} else if conf.Auth.Way == config.AuthModeUsername || conf.Auth.Way == config.AuthModeClientid {
		ledger := auth.Ledger{}
		if conf.Auth.BlacklistPath != "" {
			onError(plugin.LoadYaml(conf.Auth.BlacklistPath, &ledger), logMsg)
		}
		switch conf.Auth.Datasource {
		case config.AuthDSRedis:
			opts := rauth.Options{}
			onError(plugin.LoadYaml(conf.Auth.ConfPath, &opts), logMsg)
			onError(server.AddHook(new(rauth.Auth), &opts), logMsg)
			opts.SetBlacklist(ledger)
		case config.AuthDSMysql:
			opts := mauth.Options{}
			onError(plugin.LoadYaml(conf.Auth.ConfPath, &opts), logMsg)
			onError(server.AddHook(new(mauth.Auth), &opts), logMsg)
			opts.SetBlacklist(ledger)
		case config.AuthDSPostgresql:
			opts := pauth.Options{}
			onError(plugin.LoadYaml(conf.Auth.ConfPath, &opts), logMsg)
			onError(server.AddHook(new(pauth.Auth), &opts), logMsg)
			opts.SetBlacklist(ledger)
		case config.AuthDSHttp:
			opts := hauth.Options{}
			onError(plugin.LoadYaml(conf.Auth.ConfPath, &opts), logMsg)
			onError(server.AddHook(new(hauth.Auth), &opts), logMsg)
			opts.SetBlacklist(ledger)
		}
	} else {
		onError(config.ErrAuthWay, logMsg)
	}
}

func initStorage(server *mqtt.Server, conf *config.Config) {
	logMsg := "init storage"
	if conf.StorageWay != config.StorageWayRedis {
		onError(config.ErrStorageWay, logMsg)
	}
	err := server.AddHook(new(coredis.Storage), &coredis.Options{
		HPrefix: conf.Redis.HPrefix,
		Options: &redis.Options{
			Addr:     conf.Redis.Options.Addr,
			DB:       conf.Redis.Options.DB,
			Username: conf.Redis.Options.Username,
			Password: conf.Redis.Options.Password,
		},
	})
	onError(err, logMsg)
}

func initBridge(server *mqtt.Server, conf *config.Config) {
	logMsg := "init bridge"
	if conf.BridgeWay == config.BridgeWayNone {
		return
	} else if conf.BridgeWay == config.BridgeWayKafka {
		opts := cokafka.Options{}
		onError(plugin.LoadYaml(conf.BridgePath, &opts), logMsg)
		onError(server.AddHook(new(cokafka.Bridge), &opts), logMsg)
	}
}

func initClusterNode(server *mqtt.Server, conf *config.Config) {
	//setup member node
	agent = cs.NewAgent(&conf.Cluster)
	agent.BindMqttServer(server)
	onError(agent.Start(), "create node and join cluster")

	logger.Info().Msg("cluster node created")
}

// onError handle errors and simplify code
func onError(err error, msg string) {
	if err != nil {
		logger.Fatal().Err(err).Msg(msg)
	}
}

func ConfHandler(w http.ResponseWriter, req *http.Request) {
	body, err := json.MarshalIndent(agent.Config, "", "\t")
	if err != nil {
		io.WriteString(w, err.Error())
		return
	}

	w.Write(body)
}

func MsHandler(w http.ResponseWriter, r *http.Request) {
	body, err := json.MarshalIndent(agent.GetMemberList(), "", "\t")
	if err != nil {
		io.WriteString(w, err.Error())
		return
	}

	w.Write(body)
}

func PeerHandler(w http.ResponseWriter, r *http.Request) {
	key := strings.SplitN(r.RequestURI, "/", 4)[3]
	defer r.Body.Close()
	switch r.Method {
	case http.MethodPut:
		//val, err := io.ReadAll(r.Body)
		//if err != nil {
		//	//logger.Errorf("[http] failed to read on PUT: %v", err)
		//	http.Error(w, "Failed to PUT", http.StatusBadRequest)
		//	return
		//}

		//agent.Propose(key, string(val))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		if val := agent.GetValue(key); len(val) > 0 {
			w.Write([]byte(strings.Join(val, ",")))
		} else {
			http.Error(w, "Failed to GET", http.StatusNotFound)
		}
	case http.MethodPost:
		addr, err := io.ReadAll(r.Body)
		if err != nil {
			//logger.Errorf("[http] failed to read on POST: %v", err)
			http.Error(w, "Failed to POST", http.StatusBadRequest)
			return
		}

		nodeId, err := strconv.ParseUint(key, 0, 64)
		if err != nil {
			//logger.Errorf("[http] failed to convert ID for conf change: %v", err)
			http.Error(w, "Failed to POST", http.StatusBadRequest)
			return
		}

		agent.AddRaftPeer(string(nodeId), string(addr))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		nodeId, err := strconv.ParseUint(key, 0, 64)
		if err != nil {
			//logger.Errorf("[http] failed to convert ID for conf change: %v", err)
			http.Error(w, "Failed to POST", http.StatusBadRequest)
			return
		}

		agent.RemoveRaftPeer(string(nodeId))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Add("Allow", http.MethodPut)
		w.Header().Add("Allow", http.MethodGet)
		w.Header().Add("Allow", http.MethodPost)
		w.Header().Add("Allow", http.MethodDelete)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
