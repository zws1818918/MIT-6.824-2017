package shardkv

// import "shardmaster"
import "labrpc"
import "raft"
import "sync"
import (
	"encoding/gob"
	"log"
	"time"
	"shardmaster"
	"bytes"
)

const Debug = 1

func DPrintln(a ...interface{}) {
	if Debug > 0 {
		log.Println(a...)
	}
	return
}

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.

	Kind string
	Args interface{}
}

type Result struct {
	Kind  string
	Args  interface{}
	Reply interface{}
}

type ShardKV struct {
	mu           sync.Mutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	make_end     func(string) *labrpc.ClientEnd
	gid          int
	masters      []*labrpc.ClientEnd
	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	results map[int]chan Result
	data    [shardmaster.NShards]map[string]string
	ack     map[int64]int
	mck     *shardmaster.Clerk
	uid     int64

	cfg shardmaster.Config
}

func (kv *ShardKV) AppendLogEntry(entry Op) (Result, bool) {
	index, _, isLeader := kv.rf.Start(entry)

	if !isLeader {
		return Result{}, false
	}

	kv.mu.Lock()
	ch, ok := kv.results[index]
	if !ok {
		ch = make(chan Result, 1)
		kv.results[index] = ch
	}
	kv.mu.Unlock()

	select {
	case msg := <-ch:
		return msg, true
	case <-time.After(CLIENT_TIMEOUT):
		return Result{}, false
	}
}

func (kv *ShardKV) SendResult(index int, result Result) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	if _, ok := kv.results[index]; !ok {
		kv.results[index] = make(chan Result, 1)
	} else {
		select {
		case <-kv.results[index]:
		default:
		}
	}
	kv.results[index] <- result
}

func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	entry := Op{Kind: OP_GET, Args: *args}

	msg, ok := kv.AppendLogEntry(entry)
	if !ok {
		reply.WrongLeader = true
		return
	}

	if recArgs, ok := msg.Args.(GetArgs); !ok {
		reply.WrongLeader = true
	} else if args.ClientId != recArgs.ClientId || args.ReqId != recArgs.ReqId {
		reply.WrongLeader = true
	} else {
		*reply = msg.Reply.(GetReply)
		reply.WrongLeader = false
	}

}

func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	entry := Op{Kind: args.Op, Args: *args}

	msg, ok := kv.AppendLogEntry(entry)
	if !ok {
		reply.WrongLeader = true
		return
	}

	if recArgs, ok := msg.Args.(PutAppendArgs); !ok {
		reply.WrongLeader = true
	} else if args.ClientId != recArgs.ClientId || args.ReqId != recArgs.ReqId {
		reply.WrongLeader = true
	} else {
		reply.Err = msg.Reply.(PutAppendReply).Err
		reply.WrongLeader = false
	}
}

func (kv *ShardKV) CheckDuplicated(clientId int64, requestId int) bool {
	if value, ok := kv.ack[clientId]; ok && value >= requestId {
		return true
	}
	kv.ack[clientId] = requestId
	return false
}

func (kv *ShardKV) CheckValidKey(key string) bool {
	shardId := key2shard(key)
	if kv.gid != kv.cfg.Shards[shardId] {
		return false
	}
	return true
}

func (kv *ShardKV) ApplyOp(op *Op) interface{} {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	switch op.Args.(type) {
	case GetArgs:
		return kv.ApplyGet(op.Args.(GetArgs))
	case PutAppendArgs:
		return kv.ApplyPutAppend(op.Args.(PutAppendArgs))
	case ReconfigureArgs:
		return kv.ApplyReconfigure(op.Args.(ReconfigureArgs))
	case TransferReply:
		kv.ApplyTransferReply(op.Args.(TransferReply))
		return nil
	case NotifyArgs:
		return kv.ApplyTransferNotify(op.Args.(NotifyArgs))
	}
	return nil
}

func (kv *ShardKV) ApplyGet(args GetArgs) GetReply {
	var reply GetReply
	if !kv.CheckValidKey(args.Key) {
		reply.Err = ErrWrongGroup
		return reply
	}
	if value, ok := kv.data[key2shard(args.Key)][args.Key]; ok {
		reply.Err = OK
		reply.Value = value
	} else {
		reply.Err = ErrNoKey
	}
	DPrintln("Server", kv.gid, kv.me, "Apply get:",
		key2shard(args.Key), "->", args.Key, "->", reply.Err, reply.Value)
	return reply
}

func (kv *ShardKV) ApplyPutAppend(args PutAppendArgs) PutAppendReply {
	var reply PutAppendReply
	if !kv.CheckValidKey(args.Key) {
		reply.Err = ErrWrongGroup
		return reply
	}
	if !kv.CheckDuplicated(args.ClientId, args.ReqId) {
		if args.Op == OP_PUT {
			kv.data[key2shard(args.Key)][args.Key] = args.Value
		} else if args.Op == OP_APPEND {
			kv.data[key2shard(args.Key)][args.Key] += args.Value
		}
	}
	DPrintln("Server", kv.gid, kv.me, "Apply PutAppend:",
		key2shard(args.Key), "->", args.Key, "->", kv.data[key2shard(args.Key)][args.Key])
	reply.Err = OK
	return reply
}

func (kv *ShardKV) GenerateTransferShards(cfg *shardmaster.Config) map[int][]int {
	shards := make(map[int][]int)

	for i := 0; i < shardmaster.NShards; i++ {
		if kv.cfg.Shards[i] != kv.gid && cfg.Shards[i] == kv.gid {
			gid := kv.cfg.Shards[i]

			if gid != 0 {
				if _, ok := shards[gid]; !ok {
					shards[gid] = make([]int, 0)
				}
				shards[gid] = append(shards[gid], i)
			}
		}
	}
	return shards
}

func (kv *ShardKV) TransferShard(args *TransferArgs, reply *TransferReply) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	reply.ConfigNum = args.ConfigNum
	// may should be removed
	if _, isLeader := kv.rf.GetState(); !isLeader {
		reply.Err = ErrWrongLeader
		return
	}

	if kv.cfg.Num < args.ConfigNum {
		reply.Err = ErrNotReady
		return
	}

	reply.Err = OK

	for i := 0; i < shardmaster.NShards; i++ {
		reply.Shards[i] = make(map[string]string)
	}

	for _, shard := range args.Shards {
		for k, v := range kv.data[shard] {
			reply.Shards[shard][k] = v
		}
	}

	reply.Ack = make(map[int64]int)

	for k, v := range kv.ack {
		reply.Ack[k] = v
	}
}

func (kv *ShardKV) SendTransferShard(gid int, args *TransferArgs, reply *TransferReply) bool {
	for _, server := range kv.cfg.Groups[gid] {
		//DPrintln("server", kv.gid, kv.me, "send transfer to:", gid, server)
		srv := kv.make_end(server)
		ok := srv.Call("ShardKV.TransferShard", args, reply)
		if ok {
			//DPrintln("server", kv.gid, kv.me, "receive transfer reply from:", gid, *reply)
			if reply.Err == OK {
				return true
			} else if reply.Err == ErrNotReady {
				return false
			}
		}
	}
	return false
}

func (kv *ShardKV) BroadcastTransferShard(cfg *shardmaster.Config, transferShards map[int][]int, ret *ReconfigureArgs) bool {
	var lock sync.Mutex
	var wait sync.WaitGroup

	res := true

	for key, value := range transferShards {
		wait.Add(1)
		go func(gid int, shards []int) {
			defer wait.Done()

			var reply TransferReply
			var args TransferArgs
			args.ConfigNum = cfg.Num
			args.Shards = shards

			if kv.SendTransferShard(gid, &args, &reply) {
				kv.rf.Start(Op{Kind:OP_TRANSFER,Args:reply})
				lock.Lock()

				for index, data := range reply.Shards {
					for key, value := range data {
						ret.Shards[index][key] = value
					}
				}

				for key, value := range reply.Ack {
					ret.Ack[key] = value
				}

				lock.Unlock()
			} else {
				res = false
			}

		}(key, value)
	}
	wait.Wait()
	return res
}

func (kv *ShardKV) TransferNotify(args *NotifyArgs, reply *NotifyReply) {

	if _,isLeader := kv.rf.GetState(); !isLeader {
		reply.Err = ErrWrongLeader
		return
	}

	if kv.cfg.Num > args.ConfigNum {
		reply.Err = ErrOutDate
		return
	}

	op := Op{Kind:OP_NOTIFY,Args:*args}
	msg,ok := kv.AppendLogEntry(op)

	if !ok {
		reply.Err = ErrWrongLeader
		return
	}

	if _, ok := msg.Args.(NotifyArgs); !ok {
		reply.Err = ErrWrongLeader
	} else {
		reply.Err = msg.Reply.(NotifyReply).Err
	}
}

func (kv *ShardKV) SendTransferNotify(gid int,args *NotifyArgs, reply *NotifyReply) bool {
	for _, server := range kv.cfg.Groups[gid] {
		srv := kv.make_end(server)
		ok := srv.Call("ShardKV.TransferNotify", args, reply)
		if ok {
			if reply.Err == OK {
				return true
			} else if reply.Err == ErrOutDate {
				return false
			}
		}
	}
	return false
}

func (kv *ShardKV) BroadcastTransferNotify(cfg shardmaster.Config) {
	var args NotifyArgs
	args.Shards = make([]int,0)
	args.ConfigNum = cfg.Num
	for i:=1; i < shardmaster.NShards; i++ {
		if cfg.Shards[i] == kv.gid {
			args.Shards = append(args.Shards,i)
		}
	}

	for gid,_ := range cfg.Groups {
		if gid != kv.gid {
			go func(gid int,args *NotifyArgs) {
				var reply NotifyReply
				kv.SendTransferNotify(gid,args,&reply)
			}(gid,&args)
		}
	}
}

func (kv *ShardKV) PrepareReconfigure(cfg *shardmaster.Config) (ReconfigureArgs, bool) {
	var res ReconfigureArgs
	res.Cfg = *cfg
	res.Ack = make(map[int64]int)
	for i := 0; i < shardmaster.NShards; i++ {
		res.Shards[i] = make(map[string]string)
	}

	shards := kv.GenerateTransferShards(cfg)
	ok := kv.BroadcastTransferShard(cfg, shards, &res)
	//DPrintln("server", kv.gid, kv.me, "get reconfig:", res, ok)
	return res, ok
}

func (kv *ShardKV) SyncReconfigure(args ReconfigureArgs) bool {
	op := Op{Kind: OP_RECONFIGURE, Args: args}
	for i := 0; i < 3; i++ {
		if _, isLeader := kv.rf.GetState(); !isLeader {
			return false
		}
		//DPrintln("server", kv.gid, kv.me, "sync reconfig:", args)
		result, ok := kv.AppendLogEntry(op)
		if !ok {
			continue
		}
		if recArgs, ok := result.Args.(ReconfigureArgs); ok {
			if recArgs.Cfg.Num == args.Cfg.Num {
				return true
			}
		}
	}
	return false
}

func (kv *ShardKV) ApplyReconfigure(args ReconfigureArgs) ReconfigureReply {
	var reply ReconfigureReply

	if args.Cfg.Num > kv.cfg.Num {
		// already reached consensus, merge db and ack
		for shardIndex, data := range args.Shards {
			for k, v := range data {
				kv.data[shardIndex][k] = v
			}
		}
		for clientId := range args.Ack {
			if _, exist := kv.ack[clientId]; !exist || kv.ack[clientId] < args.Ack[clientId] {
				kv.ack[clientId] = args.Ack[clientId]
			}
		}
		kv.cfg = args.Cfg
		DPrintln("Server", kv.gid, kv.me, "Apply reconfig:", args)
		reply.Err = OK
	}
	return reply
}

func (kv *ShardKV) ApplyTransferReply(args TransferReply) {
	if args.ConfigNum == kv.cfg.Num+1 {
		// already reached consensus, merge db and ack
		for shardIndex, data := range args.Shards {
			for k, v := range data {
				kv.data[shardIndex][k] = v
			}
			kv.cfg.Shards[shardIndex] = kv.gid
		}
		for clientId := range args.Ack {
			if _, exist := kv.ack[clientId]; !exist || kv.ack[clientId] < args.Ack[clientId] {
				kv.ack[clientId] = args.Ack[clientId]
			}
		}

		DPrintln("Server", kv.gid, kv.me, "Apply transfer shard:", args)
	}
}

func (kv *ShardKV) ApplyTransferNotify(args NotifyArgs) NotifyReply {
	for _,shard := range args.Shards {
		kv.data[shard] = make(map[string]string)
	}
	DPrintln("Server", kv.gid, kv.me, "Apply transfer notify:", args)
	return NotifyReply{Err:OK}
}

func (kv *ShardKV) ReadSnapshot(snapshot []byte) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	var LastIncludedIndex int
	var LastIncludedTerm int

	kv.ack = make(map[int64]int)
	for i := 0; i < shardmaster.NShards; i++ {
		kv.data[i] = make(map[string]string)
	}

	r := bytes.NewBuffer(snapshot)
	d := gob.NewDecoder(r)
	d.Decode(&LastIncludedIndex)
	d.Decode(&LastIncludedTerm)
	d.Decode(&kv.cfg)
	d.Decode(&kv.data)
	d.Decode(&kv.ack)
	DPrintln("server", kv.gid, kv.me, "use snapshot:", kv.cfg, kv.data, kv.ack)
}

func (kv *ShardKV) TakeSnapshot(index int) {
	if kv.maxraftstate != -1 && float64(kv.rf.GetPersistSize()) > float64(kv.maxraftstate)*0.8 {
		w := new(bytes.Buffer)
		e := gob.NewEncoder(w)
		e.Encode(kv.cfg)
		e.Encode(kv.data)
		e.Encode(kv.ack)
		data := w.Bytes()
		go kv.rf.TakeSnapshot(data, index)
	}
}

//
// the tester calls Kill() when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *ShardKV) Kill() {
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *ShardKV) Init() {
	kv.uid = nrand()
	kv.mck = shardmaster.MakeClerk(kv.masters)
	kv.results = make(map[int]chan Result)
	kv.ack = make(map[int64]int)
	for i := 0; i < shardmaster.NShards; i++ {
		kv.data[i] = make(map[string]string)
	}
}

func (kv *ShardKV) Loop() {
	for {
		msg := <-kv.applyCh
		if msg.UseSnapshot {
			kv.ReadSnapshot(msg.Snapshot)
		} else {
			var result Result
			request := msg.Command.(Op)
			result.Args = request.Args
			result.Kind = request.Kind
			result.Reply = kv.ApplyOp(&request)

			kv.SendResult(msg.Index, result)
			kv.TakeSnapshot(msg.Index)

		}
	}
}

func (kv *ShardKV) Poll() {
	for {
		if _, isLeader := kv.rf.GetState(); isLeader {
			lastCfg := kv.mck.Query(-1)
			for i := kv.cfg.Num + 1; i <= lastCfg.Num; i++ {
				cfg := kv.mck.Query(i)
				args, ok := kv.PrepareReconfigure(&cfg)
				if !ok {
					break
				}
				if !kv.SyncReconfigure(args) {
					break
				}

				kv.BroadcastTransferNotify(kv.cfg)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

//
// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots with
// persister.SaveSnapshot(), and Raft should save its state (including
// log) with persister.SaveRaftState().
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardmaster.
//
// pass masters[] to shardmaster.MakeClerk() so you can send
// RPCs to the shardmaster.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use masters[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int, masters []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call gob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	gob.Register(Op{})
	gob.Register(PutAppendArgs{})
	gob.Register(GetArgs{})
	gob.Register(PutAppendReply{})
	gob.Register(GetReply{})
	gob.Register(shardmaster.Config{})
	gob.Register(ReconfigureArgs{})
	gob.Register(ReconfigureReply{})
	gob.Register(TransferArgs{})
	gob.Register(TransferReply{})
	gob.Register(NotifyArgs{})
	gob.Register(NotifyReply{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.masters = masters


	// Your initialization code here.

	// Use something like this to talk to the shardmaster:
	// kv.mck = shardmaster.MakeClerk(kv.masters)

	kv.applyCh = make(chan raft.ApplyMsg,1)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	kv.Init()

	go kv.Loop()
	go kv.Poll()

	return kv
}
