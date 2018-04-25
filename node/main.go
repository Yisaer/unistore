package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/dgraph-io/badger"
	"github.com/golang/protobuf/proto"
	"github.com/ngaut/faketikv/tikv"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"google.golang.org/grpc"
)

const (
	dbpath = "/tmp/badger"
)

var (
	MaxKey                   = []byte{}
	MinKey                   = []byte{0xFF}
	InternalKeyPrefix        = `internal\`
	InternalRegionMetaPrefix = []byte(InternalKeyPrefix + "region")
	InternalStoreMetaKey     = []byte(InternalKeyPrefix + "store")
)

func InternalRegionMetaKey(regionId uint64) []byte {
	return []byte(string(InternalRegionMetaPrefix) + strconv.FormatUint(regionId, 10))
}

func Exists(name string) (bool, error) {
	_, err := os.Stat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err != nil, err
}

type Node struct {
	clusterID uint64
	pdc       tikv.Client
	db        *badger.DB
	storeMeta metapb.Store
	// currently, we have just a region
	regions map[uint64]*metapb.Region

	tikvServer *tikv.Server
	grpcServer *grpc.Server
}

func NewNode() *Node {
	n := &Node{regions: make(map[uint64]*metapb.Region)}
	var err error
	n.storeMeta.Address = "127.0.0.1:9191"
	n.pdc, err = tikv.NewClient("127.0.0.1:2379", "", n.regions, &n.storeMeta)
	if err != nil {
		log.Fatal(err)
	}

	opts := badger.DefaultOptions
	opts.Dir = dbpath
	opts.ValueDir = dbpath
	db, err := badger.Open(opts)
	if err != nil {
		log.Fatal(err)
	}
	n.db = db
	n.clusterID = n.pdc.GetClusterID(context.TODO())
	log.Infof("cluster id %v", n.clusterID)

	return n
}

func needInit(storeMeta *metapb.Store) bool {
	return storeMeta.Id == 0
}

func (n *Node) loadMeta() {
	// Read meta from local storage
	err := n.db.View(func(txn *badger.Txn) error {
		// load storage meta
		item, err := txn.Get(InternalStoreMetaKey)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			log.Info(err)
			return err
		}
		proto.Unmarshal(val, &n.storeMeta)

		// load region meta
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := InternalRegionMetaPrefix
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			v, err := item.Value()
			if err != nil {
				return err
			}
			r := &metapb.Region{}
			proto.Unmarshal(v, r)
			n.regions[r.Id] = r
		}
		return nil
	})

	if err != nil && err != badger.ErrKeyNotFound {
		log.Fatal(err)
	}

	log.Infof("meta in local store: %+v", n)
}

func (n *Node) initStore() error {
	log.Info("initializing store")
	// allocate store id
	storeID, err := n.pdc.AllocID(context.Background())
	if err != nil {
		return err
	}
	n.storeMeta.Id = storeID

	// allocate retion id
	rid, err := n.pdc.AllocID(context.Background())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	rootRegion := &metapb.Region{
		Id:          rid,
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
		Peers:       []*metapb.Peer{&metapb.Peer{Id: rid, StoreId: n.storeMeta.Id}},
	}
	n.regions[rootRegion.Id] = rootRegion
	err = n.pdc.Bootstrap(ctx, &n.storeMeta, rootRegion)
	cancel()
	if err != nil {
		log.Fatal("Initialize failed: ", err)
	} else {
		log.Info("Initialize success")
		storeBuf, err := proto.Marshal(&n.storeMeta)
		if err != nil {
			log.Fatal("%+v", err)
		}

		err = n.db.Update(func(txn *badger.Txn) error {
			txn.Set(InternalStoreMetaKey, storeBuf)
			for rid, region := range n.regions {
				regionBuf, err := proto.Marshal(region)
				if err != nil {
					log.Fatal("%+v", err)
				}
				err = txn.Set(InternalRegionMetaKey(rid), regionBuf)
				if err != nil {
					log.Fatal("%+v", err)
				}
			}
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	return nil
}

func (n *Node) start() {
	n.loadMeta()
	if needInit(&n.storeMeta) {
		err := n.initStore()
		if err != nil {
			log.Fatal(err)
		}
	}

	err := n.pdc.PutStore(context.Background(), &n.storeMeta)
	if err != nil {
		log.Fatal(err)
	}

	n.tikvServer = tikv.NewServer(n.storeMeta, n.db)
	n.grpcServer = grpc.NewServer()
	tikvpb.RegisterTikvServer(n.grpcServer, n.tikvServer)
	l, err := net.Listen("tcp", n.storeMeta.Address)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(n.grpcServer.Serve(l))
}

func (n *Node) Close() {
	n.db.Close()
}

func main() {
	log.SetLevelByString("debug")
	n := NewNode()
	n.start()
	n.Close()
}
