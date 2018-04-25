package tikv

import (
	"github.com/dgraph-io/badger"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/errorpb"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

var _ tikvpb.TikvServer = new(Server)

type Server struct {
	mvccStore MVCCStore

	// storeMeta for current request
	storeMeta metapb.Store
	// Used for handling normal request.
	startKey []byte
	endKey   []byte
}

func NewServer(storeMeta metapb.Store, db *badger.DB) *Server {
	return &Server{
		mvccStore: MVCCStore{db: db},
		storeMeta: storeMeta,
	}
}

const requestMaxSize = 6 * 1024 * 1024

func (svr *Server) checkRequestSize(size int) *errorpb.Error {
	// TiKV has a limitation on raft log size.
	// mocktikv has no raft inside, so we check the request's size instead.
	if size >= requestMaxSize {
		return &errorpb.Error{
			RaftEntryTooLarge: &errorpb.RaftEntryTooLarge{},
		}
	}
	return nil
}

func (svr *Server) checkRequestContext(ctx *kvrpcpb.Context) *errorpb.Error {
	return nil
}

func (svr *Server) checkRequest(ctx *kvrpcpb.Context, size int) *errorpb.Error {
	if err := svr.checkRequestContext(ctx); err != nil {
		return err
	}
	return svr.checkRequestSize(size)
}

func (svr *Server) checkKeyInRegion(key []byte) bool {
	return true
}

func (svr *Server) KvGet(ctx context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	log.Debug("get", req.String())
	if !svr.checkKeyInRegion(req.Key) {
		panic("KvGet: key not in region")
	}
	val, err := svr.mvccStore.Get(req.Key, req.GetVersion())
	if err != nil {
		return &kvrpcpb.GetResponse{
			Error: convertToKeyError(err),
		}, nil
	}
	return &kvrpcpb.GetResponse{
		Value: val,
	}, nil
}

func (svr *Server) KvScan(ctx context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	log.Debug("scan", req.String())
	if !svr.checkKeyInRegion(req.GetStartKey()) {
		panic("KvScan: startKey not in region")
	}
	pairs := svr.mvccStore.Scan(req.GetStartKey(), svr.endKey, int(req.GetLimit()), req.GetVersion())
	return &kvrpcpb.ScanResponse{
		Pairs: convertToPbPairs(pairs),
	}, nil
}

func (svr *Server) KvPrewrite(ctx context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	log.Debug("prewrite", req.String())
	for _, m := range req.Mutations {
		if !svr.checkKeyInRegion(m.Key) {
			panic("KvPrewrite: key not in region")
		}
	}
	errs := svr.mvccStore.Prewrite(req.Mutations, req.PrimaryLock, req.GetStartVersion(), req.GetLockTtl())
	return &kvrpcpb.PrewriteResponse{
		Errors: convertToKeyErrors(errs),
	}, nil
}

func (svr *Server) KvCommit(ctx context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	log.Debug("commit", req.String())
	for _, k := range req.Keys {
		if !svr.checkKeyInRegion(k) {
			panic("KvCommit: key not in region")
		}
	}
	var resp kvrpcpb.CommitResponse
	err := svr.mvccStore.Commit(req.Keys, req.GetStartVersion(), req.GetCommitVersion())
	if err != nil {
		resp.Error = convertToKeyError(err)
	}
	return &resp, nil
}

func (svr *Server) KvImport(context.Context, *kvrpcpb.ImportRequest) (*kvrpcpb.ImportResponse, error) {
	return nil, nil
}

func (svr *Server) KvCleanup(ctx context.Context, req *kvrpcpb.CleanupRequest) (*kvrpcpb.CleanupResponse, error) {
	log.Debug(req.String())
	return nil, nil
}

func (svr *Server) KvBatchGet(ctx context.Context, req *kvrpcpb.BatchGetRequest) (*kvrpcpb.BatchGetResponse, error) {
	for _, k := range req.Keys {
		if !svr.checkKeyInRegion(k) {
			panic("KvBatchGet: key not in region")
		}
	}
	pairs := svr.mvccStore.BatchGet(req.Keys, req.GetVersion())
	return &kvrpcpb.BatchGetResponse{
		Pairs: convertToPbPairs(pairs),
	}, nil
}

func (svr *Server) KvBatchRollback(ctx context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	log.Debug("rollback", req.String())
	err := svr.mvccStore.Rollback(req.Keys, req.StartVersion)
	if err != nil {
		return &kvrpcpb.BatchRollbackResponse{
			Error: convertToKeyError(err),
		}, nil
	}
	return &kvrpcpb.BatchRollbackResponse{}, nil
}

func (svr *Server) KvScanLock(context.Context, *kvrpcpb.ScanLockRequest) (*kvrpcpb.ScanLockResponse, error) {
	return nil, nil
}

func (svr *Server) KvResolveLock(ctx context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	log.Debug(req.String())
	return nil, nil
}

func (svr *Server) KvGC(context.Context, *kvrpcpb.GCRequest) (*kvrpcpb.GCResponse, error) {
	return nil, nil
}

func (svr *Server) KvDeleteRange(context.Context, *kvrpcpb.DeleteRangeRequest) (*kvrpcpb.DeleteRangeResponse, error) {
	return nil, nil
}

// RawKV commands.
func (svr *Server) RawGet(context.Context, *kvrpcpb.RawGetRequest) (*kvrpcpb.RawGetResponse, error) {
	return nil, nil
}

func (svr *Server) RawPut(context.Context, *kvrpcpb.RawPutRequest) (*kvrpcpb.RawPutResponse, error) {
	return nil, nil
}

func (svr *Server) RawDelete(context.Context, *kvrpcpb.RawDeleteRequest) (*kvrpcpb.RawDeleteResponse, error) {
	return nil, nil
}

func (svr *Server) RawScan(context.Context, *kvrpcpb.RawScanRequest) (*kvrpcpb.RawScanResponse, error) {
	return nil, nil
}

func (svr *Server) RawBatchDelete(context.Context, *kvrpcpb.RawBatchDeleteRequest) (*kvrpcpb.RawBatchDeleteResponse, error) {
	return nil, nil
}

func (svr *Server) RawBatchGet(context.Context, *kvrpcpb.RawBatchGetRequest) (*kvrpcpb.RawBatchGetResponse, error) {
	return nil, nil
}

func (svr *Server) RawBatchPut(context.Context, *kvrpcpb.RawBatchPutRequest) (*kvrpcpb.RawBatchPutResponse, error) {
	return nil, nil
}

func (svr *Server) RawBatchScan(context.Context, *kvrpcpb.RawBatchScanRequest) (*kvrpcpb.RawBatchScanResponse, error) {
	return nil, nil
}

func (svr *Server) RawDeleteRange(context.Context, *kvrpcpb.RawDeleteRangeRequest) (*kvrpcpb.RawDeleteRangeResponse, error) {
	return nil, nil
}

// SQL push down commands.
func (svr *Server) Coprocessor(ctx context.Context, req *coprocessor.Request) (*coprocessor.Response, error) {
	log.Debug("cop", req.String())
	return nil, nil
}

func (svr *Server) CoprocessorStream(*coprocessor.Request, tikvpb.Tikv_CoprocessorStreamServer) error {
	return nil
}

// Raft commands (tikv <-> tikv).
func (svr *Server) Raft(tikvpb.Tikv_RaftServer) error {
	return nil
}
func (svr *Server) Snapshot(tikvpb.Tikv_SnapshotServer) error {
	return nil
}

// Region commands.
func (svr *Server) SplitRegion(ctx context.Context, req *kvrpcpb.SplitRegionRequest) (*kvrpcpb.SplitRegionResponse, error) {
	log.Debug("slitRegion", req.String())
	return &kvrpcpb.SplitRegionResponse{}, nil
}

// transaction debugger commands.
func (svr *Server) MvccGetByKey(context.Context, *kvrpcpb.MvccGetByKeyRequest) (*kvrpcpb.MvccGetByKeyResponse, error) {
	return nil, nil
}

func (svr *Server) MvccGetByStartTs(context.Context, *kvrpcpb.MvccGetByStartTsRequest) (*kvrpcpb.MvccGetByStartTsResponse, error) {
	return nil, nil
}

func convertToKeyError(err error) *kvrpcpb.KeyError {
	if locked, ok := errors.Cause(err).(*ErrLocked); ok {
		return &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				Key:         locked.Key.Raw(),
				PrimaryLock: locked.Primary,
				LockVersion: locked.StartTS,
				LockTtl:     locked.TTL,
			},
		}
	}
	if retryable, ok := errors.Cause(err).(ErrRetryable); ok {
		return &kvrpcpb.KeyError{
			Retryable: retryable.Error(),
		}
	}
	return &kvrpcpb.KeyError{
		Abort: err.Error(),
	}
}

func convertToKeyErrors(errs []error) []*kvrpcpb.KeyError {
	var keyErrors = make([]*kvrpcpb.KeyError, 0)
	for _, err := range errs {
		if err != nil {
			keyErrors = append(keyErrors, convertToKeyError(err))
		}
	}
	return keyErrors
}

func convertToPbPairs(pairs []Pair) []*kvrpcpb.KvPair {
	kvPairs := make([]*kvrpcpb.KvPair, 0, len(pairs))
	for _, p := range pairs {
		var kvPair *kvrpcpb.KvPair
		if p.Err == nil {
			kvPair = &kvrpcpb.KvPair{
				Key:   p.Key,
				Value: p.Value,
			}
		} else {
			kvPair = &kvrpcpb.KvPair{
				Error: convertToKeyError(p.Err),
			}
		}
		kvPairs = append(kvPairs, kvPair)
	}
	return kvPairs
}
