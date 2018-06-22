/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/terminal"

	log "github.com/sirupsen/logrus"
	"gitlab.com/thunderdb/ThunderDB/common"
	"gitlab.com/thunderdb/ThunderDB/conf"
	"gitlab.com/thunderdb/ThunderDB/route"
	"gitlab.com/thunderdb/ThunderDB/rpc"
	"gitlab.com/thunderdb/ThunderDB/utils"
)

const logo = `
 _____ _                     _           ____  ____
|_   _| |__  _   _ _ __   __| | ___ _ __|  _ \| __ )
  | | | '_ \| | | | '_ \ / _' |/ _ \ '__| | | |  _ \
  | | | | | | |_| | | | | (_| |  __/ |  | |_| | |_) |
  |_| |_| |_|\__,_|_| |_|\__,_|\___|_|  |____/|____/

`

var (
	version = "1"
	commit  = "unknown"
	branch  = "unknown"
)

var (
	// api
	publishPeersTimeout time.Duration
	publishPeersDelay   time.Duration

	// profile
	cpuProfile string
	memProfile string

	// key path
	privateKeyPath     string
	publicKeyStorePath string

	// other
	noLogo      bool
	showVersion bool

	// rpc server
	rpcServer *rpc.Server
)

// Role specifies the role of this app, which can be "miner", "blockProducer"
const Role = common.BlockProducer

const name = `thunderdbd`
const desc = `ThunderDB is a database`

func init() {
	flag.BoolVar(&noLogo, "nologo", false, "Do not print logo")
	flag.BoolVar(&showVersion, "version", false, "Show version information and exit")
	flag.DurationVar(&publishPeersTimeout, "publish-peers-timeout", time.Second*30, "Timeout for peers to publish")
	flag.DurationVar(&publishPeersDelay, "publish-peers-delay", time.Second, "Interval for peers publishing retry")
	flag.StringVar(&privateKeyPath, "private-key-path", "./private.key", "Path to private key file")
	flag.StringVar(&publicKeyStorePath, "public-keystore-path", "./public.keystore", "Path to public keystore file")
	flag.StringVar(&cpuProfile, "cpu-profile", "", "Path to file for CPU profiling information")
	flag.StringVar(&memProfile, "mem-profile", "", "Path to file for memory profiling information")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "\n%s\n\n", desc)
		fmt.Fprintf(os.Stderr, "Usage: %s [arguments] <data directory>\n", name)
		flag.PrintDefaults()
	}
}

func initLogs() {
	log.SetOutput(os.Stderr)
	log.SetLevel(log.InfoLevel)

	log.Infof("%s starting, version %s, commit %s, branch %s", name, version, commit, branch)
	log.Infof("%s, target architecture is %s, operating system target is %s", runtime.Version(), runtime.GOARCH, runtime.GOOS)
	log.Infof("role: %s", conf.Role)
}

func main() {
	flag.Parse()

	// init log
	initLogs()

	if showVersion {
		log.Infof("%s %s %s %s %s (commit %s, branch %s)",
			name, version, runtime.GOOS, runtime.GOARCH, runtime.Version(), commit, branch)
		os.Exit(0)
	}

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(1)
	}

	if !noLogo {
		fmt.Print(logo)
	}

	// set random
	rand.Seed(time.Now().UnixNano())

	// init profile, if cpuProfile, memProfile length is 0, nothing will be done
	utils.StartProfile(cpuProfile, memProfile)
	defer utils.StopProfile()

	// read master key
	fmt.Print("Type in Master key to continue: ")
	masterKeyBytes, err := terminal.ReadPassword(int(syscall.Stdin))
	if err != nil {
		fmt.Printf("Failed to read Master Key: %v", err)
	}
	fmt.Println("")

	// start RPC server
	rpcServer := rpc.NewServer()

	// if any error, log.Fatal will call os.Exit
	err = rpcServer.InitRPCServer("0.0.0.0:2120", privateKeyPath, masterKeyBytes)
	if err != nil {
		log.Fatalf("rpcServer.InitRPCServer failed: %s", err)
	}

	//privateKey, err := kms.GetLocalPrivateKey()
	//if err != nil {
	//	log.Fatalf("get local private key failed: %s", err)
	//}
	//
	//publicKey, err := kms.GetLocalPublicKey()
	//if err != nil {
	//	log.Fatalf("get local public key failed: %s", err)
	//}
	//
	//peers, err := InitKayakPeers(privateKey, publicKey)
	//if err != nil {
	//	log.Fatalf("init kayak peers failed: %s", err)
	//}
	//
	//kayak.NewRuntime(&kayak.TwoPCConfig{
	//	RuntimeConfig: kayak.RuntimeConfig{
	//		RootDir:        "",
	//		LocalID:        "",
	//		Runner:         nil,
	//		Transport:      nil,
	//		ProcessTimeout: 0,
	//		AutoBanCount:   0,
	//		Logger:         nil,
	//	},
	//	LogCodec:        nil,
	//	Storage:         nil,
	//	PrepareTimeout:  time.Millisecond * 500,
	//	CommitTimeout:   time.Millisecond * 500,
	//	RollbackTimeout: time.Millisecond * 500,
	//}, peers)

	// Register service by a name
	dht, err := route.NewDHTService(publicKeyStorePath, true)
	if err != nil {
		log.Fatalf("creating dht service failed: %s", err)
	}
	rpcServer.RegisterService("DHT", dht)
	rpcServer.Serve()

	log.Info("server stopped")
}
