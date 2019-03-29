package raftstore

import (
	"encoding/hex"
	"github.com/coocood/badger"
	"github.com/ngaut/log"
	"github.com/ngaut/unistore/lockstore"
	"github.com/ngaut/unistore/pd"
	"github.com/pingcap/kvproto/pkg/eraftpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"sync"
	"time"
)

type taskType int64

const (
	taskTypeStop        taskType = 0
	taskTypeRaftLogGC   taskType = 1
	taskTypeSplitCheck  taskType = 2
	taskTypeComputeHash taskType = 3

	taskTypePDAskSplit         taskType = 101
	taskTypePDAskBatchSplit    taskType = 102
	taskTypePDHeartbeat        taskType = 103
	taskTypePDStoreHeartbeat   taskType = 104
	taskTypePDReportBatchSplit taskType = 105
	taskTypePDValidatePeer     taskType = 106
	taskTypePDReadStats        taskType = 107
	tasktypePDDestroyPeer      taskType = 108

	taskTypeCompact         taskType = 201
	taskTypeCheckAndCompact taskType = 202

	taskTypeRegionGen   taskType = 401
	taskTypeRegionApply taskType = 402
	/// Destroy data between [start_key, end_key).
	///
	/// The deletion may and may not succeed.
	taskTypeRegionDestroy taskType = 403
)

type task struct {
	tp   taskType
	data interface{}
}

type regionTask struct {
	regionId uint64
	notifier chan<- *eraftpb.Snapshot
	status   *JobStatus
	startKey []byte
	endKey   []byte
}

type raftLogGCTask struct {
	raftEngine *badger.DB
	regionID   uint64
	startIdx   uint64
	endIdx     uint64
}

type splitCheckTask struct {
	region    *metapb.Region
	autoSplit bool
	policy    pdpb.CheckPolicy
}

type computeHashTask struct {
	index  uint64
	region *metapb.Region
	snap   *DBSnapshot
}

type pdAskSplitTask struct {
	region   *metapb.Region
	splitKey []byte
	peer     *metapb.Peer
	// If true, right Region derives origin region_id.
	rightDerive bool
	callback    Callback
}

type pdAskBatchSplitTask struct {
	region    *metapb.Region
	splitKeys [][]byte
	peer      *metapb.Peer
	// If true, right Region derives origin region_id.
	rightDerive bool
	callback    Callback
}

type pdRegionHeartbeatTask struct {
	region          *metapb.Region
	peer            *metapb.Peer
	downPeers       []*pdpb.PeerStats
	pendingPeers    []*metapb.Peer
	writtenBytes    uint64
	writtenKeys     uint64
	approximateSize *uint64
	approximateKeys *uint64
}

type pdStoreHeartbeatTask struct {
	stats    *pdpb.StoreStats
	engine   *badger.DB
	path     string
	capacity uint64
}

type pdReportBatchSplitTask struct {
	regions []*metapb.Region
}

type pdValidatePeerTask struct {
	region      *metapb.Region
	peer        *metapb.Peer
	mergeSource *uint64
}

type readStats map[uint64]flowStats

type flowStats struct {
	readBytes uint64
	readKeys  uint64
}

type compactTask struct {
	keyRange keyRange
}

type checkAndCompactTask struct {
	ranges                    []keyRange
	tombStoneNumThreshold     uint64 // The minimum RocksDB tombstones a range that need compacting has
	tombStonePercentThreshold uint64
}

type worker struct {
	name      string
	scheduler chan<- task
	receiver  <-chan task
	closeCh   chan struct{}
	wg        *sync.WaitGroup
}

type taskRunner interface {
	run(t task)
}

func (w *worker) start(runner taskRunner) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			task := <-w.receiver
			if task.tp == taskTypeStop {
				return
			}
			runner.run(task)
		}
	}()
}

const defaultWorkerCapacity = 128

func newWorker(name string, wg *sync.WaitGroup) *worker {
	ch := make(chan task, defaultWorkerCapacity)
	return &worker{
		scheduler: (chan<- task)(ch),
		receiver:  (<-chan task)(ch),
		name:      name,
		wg:        wg,
	}
}

type splitCheckKeyEntry struct {
	key       []byte
	pos       uint64
	valueSize uint64
	cf        string
}

type splitCheckRunner struct {
	engine          *badger.DB
	router          *router
	coprocessorHost *CoprocessorHost
}

/// run checks a region with split checkers to produce split keys and generates split admin command.
func (r *splitCheckRunner) run(t task) {
	spCheckTask := t.data.(*splitCheckTask)
	region := spCheckTask.region
	regionId := region.Id
	startKey := EncStartKey(region)
	endKey := EncEndKey(region)
	log.Debugf("executing task: [regionId: %d, startKey: %s, endKey: %s]", regionId,
		hex.EncodeToString(startKey), hex.EncodeToString(endKey))
	host := r.coprocessorHost.newSplitCheckerHost(region, r.engine, spCheckTask.autoSplit,
		spCheckTask.policy)
	if host.skip() {
		log.Debugf("skip split check, [regionId : %d]", regionId)
		return
	}
	var keys [][]byte
	var err error
	switch host.policy() {
	case pdpb.CheckPolicy_SCAN:
		if keys, err = r.scanSplitKeys(host, region, startKey, endKey); err != nil {
			log.Errorf("failed to scan split key: [regionId: %d, err: %v]", regionId, err)
			return
		}
	case pdpb.CheckPolicy_APPROXIMATE:
		if keys, err = host.approximateSplitKeys(region, r.engine); err != nil {
			log.Errorf("failed to get approximate split key, try scan way: [regionId: %d, err : %v]",
				regionId, err)
			if keys, err = r.scanSplitKeys(host, region, startKey, endKey); err != nil {
				log.Errorf("failed to scan split key: [regionId: %d, err: %v]", regionId, err)
				return
			}
		} else {
			for i, k := range keys {
				keys[i] = OriginKey(k)
			}
		}
	}
	if len(keys) != 0 {
		regionEpoch := region.GetRegionEpoch()
		msg := Msg{
			Type: MsgTypeSplitRegion,
			RegionID: regionId,
			Data: &MsgSplitRegion{
				RegionEpoch:regionEpoch,
				SplitKeys: keys,
				Callback: EmptyCallback,
			},
		}
		err = r.router.send(regionId, msg)
		if err != nil {
			log.Warnf("failed to send check result: [regionId: %d, err: %v]", regionId, err)
		}
	} else {
		log.Debugf("no need to send, split key not found: [regionId: %v]", regionId)
	}
}

/// scanSplitKeys gets the split keys by scanning the range.
func (r *splitCheckRunner) scanSplitKeys(spCheckerHost *splitCheckerHost, region *metapb.Region,
	startKey []byte, endKey []byte) ([][]byte, error) {
	/// Todo, currently it is a place holder
	return nil, nil
}

type pendingDeleteRanges struct {
	ranges *lockstore.MemStore
}

type snapContext struct {
	engiens             *Engines
	batchSize           int
	mgr                 *SnapManager
	cleanStalePeerDelay time.Duration
	pendingDeleteRanges *pendingDeleteRanges
}

type regionRunner struct {
	ctx *snapContext
	// we may delay some apply tasks if level 0 files to write stall threshold,
	// pending_applies records all delayed apply task, and will check again later
	pendingApplies []task
}

func newRegionRunner(engines *Engines, mgr *SnapManager, batchSize uint64, cleanStalePeerDelay time.Duration) *regionRunner {
	return nil // TODO: stub
}

func (r *regionRunner) run(t task) {
	// TODO: stub
}

type raftLogGCRunner struct {
}

func (r *raftLogGCRunner) run(t task) {
	// TODO: stub
}

type compactRunner struct {
	engine *badger.DB
}

func (r *compactRunner) run(t task) {
	// TODO: stub
}

type pdRunner struct {
	storeID   uint64
	pdClient  pd.Client
	router    *router
	db        *badger.DB
	scheduler chan<- task
}

func newPDRunner(storeID uint64, pdClient pd.Client, router *router, db *badger.DB, scheduler chan<- task) *pdRunner {
	return nil // TODO: stub
}

func (r *pdRunner) run(t task) {
	// TODO: stub
}

type computeHashRunner struct {
	router *router
}

func (r *computeHashRunner) run(t task) {
	// TODO: stub
}