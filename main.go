package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	id := os.Getenv("ID")
	addr := os.Getenv("ADDR")
	serversList := os.Getenv("SERVERS")
	flag.Parse()

	if id == "" || serversList == "" {
		flag.Usage()
		return
	}

	s := strings.Split(serversList, ",")
	servers := make([]Server, 0)
	for _, server := range s {
		idAddr := strings.Split(server, ";")
		if len(idAddr) < 2 {
			flag.Usage()
			return
		}
		servers = append(servers, Server{addr: idAddr[0], id: idAddr[1]})
	}

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	logger.SetLevel(logrus.InfoLevel)

	l := NewLocky(context.Background(), addr, servers, Server{addr, id}, logger)

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	fmt.Println("Blocking, press ctrl+c to exit...")
	ticker := time.NewTicker(1 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	go l.RunEvery(ctx, 5*time.Second, func() {
		fmt.Printf("This is only running on the leader node!!!\n")
	})

	for {
		select {
		case <-done:
			fmt.Println("Exiting...")
			l.Close()
			cancel()
			return
		case <-ticker.C:
			st := l.GetState()
			fmt.Printf("\r%s Leader:%s %d\n", l.localServer.id, st.leader, st.state)
		}
	}

}
