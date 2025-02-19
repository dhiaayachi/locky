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

var l service.LockyServiceServer = &Locky{}

type Locky struct {
	service.UnimplementedLockyServiceServer
	cancel      context.CancelFunc
	state       atomic.Pointer[State]
	servers     map[string]Server
	localServer Server
	stateCh     chan State
	leaderCh    chan bool
	logger      *logrus.Logger
}

func (l *Locky) GetState() *State {
	state := l.state.Load()
	l.logger.Tracef("GetState: leader=%s, state=%d", state.leader, state.state)
	return state
}

type Server struct {
	addr string
	id   string
}

type State struct {
	leader string
	state  int32
}

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

func (l *Locky) AskHealth(_ context.Context, request *service.AskHealthRequest) (*service.AskHealthResponse, error) {
	l.logger.Tracef("AskHealth: received request from %s", request.Id)
	if l.isLeader() {
		l.logger.Debug("AskHealth: Node is leader; reporting healthy")
		l.leaderCh <- true
		return &service.AskHealthResponse{Ok: true}, nil
	}
	l.logger.Debug("AskHealth: Node is not leader; reporting not healthy")
	return &service.AskHealthResponse{Ok: false}, nil
}

func (l *Locky) AskVote(_ context.Context, request *service.AskVoteRequest) (*service.AskVoteResponse, error) {
	l.logger.Tracef("AskVote: received vote request from %s", request.Id)
	st := l.GetState()
	l.logger.Debugf("AskVote: current state candidate=%v, leader=%s", st.state == StateCandidate, st.leader)
	if st.state == StateCandidate {
		l.logger.Info("AskVote: granting vote as candidate, converting to follower")
		l.stateCh <- State{leader: request.Id, state: StateFollower}
		return &service.AskVoteResponse{Granted: true, Id: l.localServer.id}, nil
	}
	if st.leader == request.Id {
		l.logger.Debug("AskVote: request from current leader, granting vote")
		return &service.AskVoteResponse{Granted: true, Id: st.leader}, nil
	}
	l.logger.Debug("AskVote: vote not granted")
	return &service.AskVoteResponse{Granted: false, Id: l.localServer.id, Leader: st.leader}, nil
}

func NewLocky(ctx context.Context, listenAddr string, servers []Server, local Server, logger *logrus.Logger) *Locky {
	sm := make(map[string]Server)
	for _, s := range servers {
		sm[s.id] = s
	}
	l := Locky{
		servers:     sm,
		localServer: local,
		stateCh:     make(chan State),
		leaderCh:    make(chan bool),
		logger:      logger,
	}
	l.state.Store(&State{leader: "", state: StateCandidate})
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

func (l *Locky) Close() {
	l.logger.Info("Closing Locky service")
	l.cancel()
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
	l.logger.Debugf("runFollower: sleeping for %v", sleepDuration)
	time.Sleep(sleepDuration)
	st := l.GetState()
	l.logger.Tracef("runFollower: state is leader=%s", st.leader)
	lead, ok := l.servers[st.leader]
	if !ok {
		l.logger.Warnf("runFollower: leader %s not found; transitioning to candidate", st.leader)
		l.stateCh <- State{leader: "", state: StateCandidate}
		return
	}

	health, err := l.askHealth(lead, ctx)
	if err != nil || !health.Ok {
		l.logger.Warnf("runFollower: health check failed for leader %s (error=%v, ok=%v); transitioning to candidate", st.leader, err, health)
		l.stateCh <- State{leader: "", state: StateCandidate}
	} else {
		l.logger.Debugf("runFollower: health check succeeded for leader %s", st.leader)
	}
}

func (l *Locky) runLeader(ctx context.Context) {
	ch := time.After(LeaderLeaseTimeout)
	count := 1
	quorum := len(l.servers)/2 + 1
	l.logger.Debugf("runLeader: leader loop started; quorum=%d", quorum)
	for {
		select {
		case <-ctx.Done():
			l.logger.Info("runLeader: context done, exiting leader loop")
			return
		case <-ch:
			l.logger.Warn("runLeader: leader lease timeout reached; transitioning to candidate")
			l.stateCh <- State{leader: "", state: StateCandidate}
			return
		case <-l.leaderCh:
			count++
			l.logger.Tracef("runLeader: received heartbeat, count=%d", count)
			if count >= quorum {
				l.logger.Info("runLeader: quorum achieved; remaining as leader")
				return
			}
		}
	}
}

func (l *Locky) runCandidate(ctx context.Context) {
	sleepDuration := time.Duration(rand.Intn(50))*time.Millisecond + CandidateCoalesceTimeout
	l.logger.Debugf("runCandidate: sleeping for %v", sleepDuration)
	time.Sleep(sleepDuration)
	votes := 1
	quorum := len(l.servers)/2 + 1
	l.logger.Info("runCandidate: starting election, initial vote count = 1")
	for _, s := range l.servers {
		if l.localServer.id == s.id {
			continue
		}
		l.logger.Tracef("runCandidate: requesting vote from server %s", s.id)
		vote, err := l.askVote(s, ctx)
		if err != nil {
			l.logger.Errorf("runCandidate: error asking vote from server %s: %v", s.id, err)
			continue
		}
		if vote.Granted {
			votes++
			l.logger.Debugf("runCandidate: received vote from server %s, total votes now %d", s.id, votes)
		} else {
			if vote.Leader != "" && vote.Leader != l.localServer.id {
				l.logger.Infof("runCandidate: another leader (%s) detected; transitioning to follower", vote.Leader)
				l.stateCh <- State{leader: vote.Leader, state: StateFollower}
				return
			}
			l.logger.Debugf("runCandidate: vote not granted by server %s", s.id)
		}
		if votes >= quorum {
			st := l.GetState()
			if st.state == StateCandidate {
				l.logger.Info("runCandidate: election won; transitioning to leader")
				l.stateCh <- State{leader: l.localServer.id, state: StateLeader}
				return
			}
		}
	}
	l.logger.Warn("runCandidate: election did not reach quorum, remaining candidate")
}

func (l *Locky) askVote(s Server, ctx context.Context) (*service.AskVoteResponse, error) {
	l.logger.Tracef("askVote: connecting to server %s at %s", s.id, s.addr)
	conn, err := grpc.NewClient(s.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		l.logger.Errorf("askVote: failed to connect to server %s: %v", s.id, err)
		return nil, err
	}
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
		l.logger.Tracef("askVote: closed connection to server %s", s.id)
	}(conn)
	ctx2, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	sc := service.NewLockyServiceClient(conn)
	resp, err := sc.AskVote(ctx2, &service.AskVoteRequest{Id: l.localServer.id})
	if err != nil {
		l.logger.Errorf("askVote: RPC error to server %s: %v", s.id, err)
	} else {
		l.logger.Debugf("askVote: received response from server %s: Granted=%v", s.id, resp.Granted)
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
		l.logger.Tracef("askHealth: closed connection to server %s", s.id)
	}(conn)
	ctx2, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	sc := service.NewLockyServiceClient(conn)
	resp, err := sc.AskHealth(ctx2, &service.AskHealthRequest{Id: l.localServer.id, Leader: l.GetLeader()})
	if err != nil {
		l.logger.Errorf("askHealth: RPC error to server %s: %v", s.id, err)
	} else {
		l.logger.Debugf("askHealth: received response from server %s: Ok=%v", s.id, resp.Ok)
	}
	return resp, err
}

func (l *Locky) GetLeader() string {
	leader := l.GetState().leader
	l.logger.Tracef("GetLeader: returning leader %s", leader)
	return leader
}

func (l *Locky) isLeader() bool {
	isLeader := l.state.Load().state == StateLeader
	l.logger.Tracef("isLeader: %v", isLeader)
	return isLeader
}

func (l *Locky) RunOnce(f func()) {
	if l.isLeader() {
		l.logger.Debug("RunOnce: executing function as leader")
		f()
	} else {
		l.logger.Trace("RunOnce: not leader, skipping execution")
	}
}

func (l *Locky) RunEvery(ctx context.Context, d time.Duration, f func()) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.logger.Debug("RunEvery: context done, stopping periodic execution")
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
