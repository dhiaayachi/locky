package main

import (
	"context"
	"math/rand"
	"net"
	"sync/atomic"
	"time"

	"github.com/dhiaayachi/locky/proto/gen/service/v1"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	LeaderLeaseTimeout       = time.Millisecond * 100
	CandidateCoalesceTimeout = time.Millisecond * 10
	FollowerCoalesceTimeout  = time.Millisecond * 10
)

const (
	StateCandidate = 0
	StateLeader    = 1
	StateFollower  = 2
)

var _ service.LockyServiceServer = &Locky{}

type Locky struct {
	service.UnimplementedLockyServiceServer
	cancel      context.CancelFunc
	servers     map[string]Server
	localServer Server
	logger      *logrus.Logger
	state       atomic.Pointer[State]
	stateCh     chan State
}

type Server struct {
	addr string
	id   string
}

type State struct {
	leader string
	state  int32
}

func NewLocky(ctx context.Context, listenAddr string, servers []Server, local Server, logger *logrus.Logger) *Locky {
	sm := make(map[string]Server)
	for _, s := range servers {
		sm[s.id] = s
	}
	l := Locky{
		servers:     sm,
		localServer: local,
		logger:      logger,
	}

	srv := grpc.NewServer()
	srv.RegisterService(&service.LockyService_ServiceDesc, &l)
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		l.logger.Errorf("Failed to listen on %s: %v", listenAddr, err)
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			l.logger.Debug("Starting gRPC server Serve loop")
			err = srv.Serve(lis)
			if err != nil {
				l.logger.Errorf("gRPC server error: %v", err)
				panic(err)
			}
			after := time.After(time.Second)
			select {
			case <-after:
			case <-ctx.Done():
				l.logger.Info("gRPC server loop context canceled, exiting")
				return
			}
		}
	}()
	l.cancel = cancel
	go l.runState(ctx)
	go l.run(ctx)

	return &l
}

func (l *Locky) AskVote(ctx context.Context, request *service.AskVoteRequest) (*service.AskVoteResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (l *Locky) AskHealth(ctx context.Context, request *service.AskHealthRequest) (*service.AskHealthResponse, error) {
	//TODO implement me
	panic("implement me")
}

func (l *Locky) Close() {
	l.logger.Info("Closing Locky service")
	l.cancel()
}

func (l *Locky) RunOnce(f func()) {
	if l.isLeader() {
		f()
	}
}

func (l *Locky) RunEvery(ctx context.Context, d time.Duration, f func()) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if l.isLeader() {
				l.logger.Trace("RunEvery: executing periodic function as leader")
				f()
			} else {
				l.logger.Trace("RunEvery: not leader, skipping periodic execution")
			}
		}
	}
}

func (l *Locky) GetLeader() string {
	panic("not implemented")
}

func (l *Locky) GetState() *State {
	state := l.state.Load()
	l.logger.Tracef("GetState: leader=%s, state=%d", state.leader, state.state)
	return state
}

func (l *Locky) runState(ctx context.Context) {
	l.logger.Trace("runState: starting state loop")
	for {
		select {
		case <-ctx.Done():
			l.logger.Debug("runState: context done, exiting loop")
			return
		case s := <-l.stateCh:
			l.logger.Debugf("runState: updating state to leader=%s, state=%d", s.leader, s.state)
			l.state.Store(&s)
		}
	}
}

func (l *Locky) run(ctx context.Context) {
	l.logger.Trace("run: starting main loop")
	for {
		select {
		default:
			st := l.GetState()
			l.logger.Tracef("run: current state: leader=%s, state=%d", st.leader, st.state)
			switch st.state {
			case StateCandidate:
				l.logger.Trace("run: entering candidate loop")
				l.runCandidate(ctx)
			case StateLeader:
				l.logger.Trace("run: entering leader loop")
				l.runLeader(ctx)
			case StateFollower:
				l.logger.Trace("run: entering follower loop")
				l.runFollower(ctx)
			}
		}
	}
}

func (l *Locky) runFollower(ctx context.Context) {
	sleepDuration := time.Duration(rand.Intn(50))*time.Millisecond + FollowerCoalesceTimeout
	time.Sleep(sleepDuration)
}

func (l *Locky) runLeader(ctx context.Context) {
	sleepDuration := time.Duration(rand.Intn(50))*time.Millisecond + LeaderLeaseTimeout
	time.Sleep(sleepDuration)
}

func (l *Locky) runCandidate(ctx context.Context) {
	sleepDuration := time.Duration(rand.Intn(50))*time.Millisecond + CandidateCoalesceTimeout
	time.Sleep(sleepDuration)
}

func (l *Locky) askVote(s Server, ctx context.Context) (*service.AskVoteResponse, error) {
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		l.logger.Errorf("askVote: failed to connect to server %s: %v", s.id, err)
		return nil, err
	}
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)
	ctx2, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	sc := service.NewLockyServiceClient(conn)
	resp, err := sc.AskVote(ctx2, &service.AskVoteRequest{Id: l.localServer.id})
	if err != nil {
		l.logger.Errorf("askVote: RPC error to server %s: %v", s.id, err)
	}
	return resp, err
}

func (l *Locky) askHealth(s Server, ctx context.Context) (*service.AskHealthResponse, error) {
	l.logger.Tracef("askHealth: connecting to server %s at %s", s.id, s.addr)
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		l.logger.Errorf("askHealth: failed to connect to server %s: %v", s.id, err)
		return nil, err
	}
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)
	ctx2, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	sc := service.NewLockyServiceClient(conn)
	resp, err := sc.AskHealth(ctx2, &service.AskHealthRequest{Id: l.localServer.id, Leader: l.GetLeader()})
	if err != nil {
		l.logger.Errorf("askHealth: RPC error to server %s: %v", s.id, err)
	}
	return resp, err
}

func (l *Locky) isLeader() bool {
	isLeader := l.state.Load().state == StateLeader
	l.logger.Tracef("isLeader: %v", isLeader)
	return isLeader
}
