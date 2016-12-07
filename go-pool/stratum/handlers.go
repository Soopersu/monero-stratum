package stratum

import (
	"log"
	"regexp"
	"strings"
	"sync/atomic"

	"../util"
)

var noncePattern *regexp.Regexp

const defaultWorkerId = "0"

func init() {
	noncePattern, _ = regexp.Compile("^[0-9a-f]{8}$")
}

func (s *StratumServer) handleLoginRPC(cs *Session, e *Endpoint, params *LoginParams) (*JobReply, *ErrorReply) {
	if !s.config.BypassAddressValidation && !util.ValidateAddress(params.Login, s.config.Address) {
		return nil, &ErrorReply{Code: -1, Message: "Invalid address used for login"}
	}

	t := s.currentBlockTemplate()
	if t == nil {
		return nil, &ErrorReply{Code: -1, Message: "Job not ready"}
	}

	id := extractWorkerId(params.Login)
	miner, ok := s.miners.Get(id)
	if !ok {
		miner = NewMiner(id, cs.ip)
		s.registerMiner(miner)
	}

	log.Printf("Miner connected %s@%s", id, cs.ip)

	s.registerSession(cs)
	miner.heartbeat()

	return &JobReply{Id: id, Job: cs.getJob(t), Status: "OK"}, nil
}

func (s *StratumServer) handleGetJobRPC(cs *Session, e *Endpoint, params *GetJobParams) (*JobReplyData, *ErrorReply) {
	miner, ok := s.miners.Get(params.Id)
	if !ok {
		return nil, &ErrorReply{Code: -1, Message: "Unauthenticated"}
	}
	t := s.currentBlockTemplate()
	if t == nil {
		return nil, &ErrorReply{Code: -1, Message: "Job not ready"}
	}
	miner.heartbeat()
	return cs.getJob(t), nil
}

func (s *StratumServer) handleSubmitRPC(cs *Session, e *Endpoint, params *SubmitParams) (*SubmitReply, *ErrorReply) {
	miner, ok := s.miners.Get(params.Id)
	if !ok {
		return nil, &ErrorReply{Code: -1, Message: "Unauthenticated"}
	}
	miner.heartbeat()

	job := cs.findJob(params.JobId)
	if job == nil {
		return nil, &ErrorReply{Code: -1, Message: "Invalid job id"}
	}

	if !noncePattern.MatchString(params.Nonce) {
		return nil, &ErrorReply{Code: -1, Message: "Malformed nonce"}
	}
	nonce := strings.ToLower(params.Nonce)
	exist := job.submit(nonce)
	if exist {
		atomic.AddUint64(&miner.invalidShares, 1)
		return nil, &ErrorReply{Code: -1, Message: "Duplicate share"}
	}

	t := s.currentBlockTemplate()
	if job.height != t.Height {
		log.Printf("Stale share for height %d from %s@%s", job.height, miner.id, miner.ip)
		atomic.AddUint64(&miner.staleShares, 1)
		return nil, &ErrorReply{Code: -1, Message: "Block expired"}
	}

	validShare := miner.processShare(s, e, job, t, nonce, params.Result)
	if !validShare {
		return nil, &ErrorReply{Code: -1, Message: "Low difficulty share"}
	}
	return &SubmitReply{Status: "OK"}, nil
}

func (s *StratumServer) handleUnknownRPC(cs *Session, req *JSONRpcReq) *ErrorReply {
	log.Printf("Unknown RPC method: %v", req)
	return &ErrorReply{Code: -1, Message: "Invalid method"}
}

func (s *StratumServer) broadcastNewJobs() {
	t := s.currentBlockTemplate()
	if t == nil {
		return
	}
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	count := len(s.sessions)
	log.Printf("Broadcasting new jobs to %d miners", count)
	bcast := make(chan int, 1024*16)
	n := 0

	for m := range s.sessions {
		n++
		bcast <- n
		go func(cs *Session) {
			reply := cs.getJob(t)
			err := cs.pushMessage("job", &reply)
			<-bcast
			if err != nil {
				log.Printf("Job transmit error to %s: %v", cs.ip, err)
				s.removeSession(cs)
			} else {
				s.setDeadline(cs.conn)
			}
		}(m)
	}
}

func (s *StratumServer) refreshBlockTemplate(bcast bool) {
	newBlock := s.fetchBlockTemplate()
	if newBlock && bcast {
		s.broadcastNewJobs()
	}
}

func extractWorkerId(loginWorkerPair string) string {
	parts := strings.SplitN(loginWorkerPair, ".", 2)
	if len(parts) > 1 {
		return parts[1]
	}
	return defaultWorkerId
}
