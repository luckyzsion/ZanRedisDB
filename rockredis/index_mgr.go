package rockredis

import (
	"bytes"
	"errors"
	"github.com/absolute8511/ZanRedisDB/common"
	"github.com/absolute8511/gorocksdb"
	"sync"
)

var (
	ErrIndexStateInvalidChange = errors.New("index state change state is not invalid")
	ErrIndexDeleteNotInDeleted = errors.New("delete index in wrong state")
	ErrIndexClosed             = errors.New("index is closed")
)

var (
	buildIndexBlock = 1000
)

type JsonIndex struct {
}

type TableIndexContainer struct {
	sync.RWMutex
	// field -> index name, to convert "secondaryindex.select * from table where field = xxx" to scan(/hindex/table/indexname/xxx)
	hsetIndexes map[string]*HsetIndex
	jsonIndexes map[string]*JsonIndex
}

func NewIndexContainer() *TableIndexContainer {
	return &TableIndexContainer{
		hsetIndexes: make(map[string]*HsetIndex),
		jsonIndexes: make(map[string]*JsonIndex),
	}
}

func (self *TableIndexContainer) marshalHsetIndexes() ([]byte, error) {
	var indexList HsetIndexList
	for _, v := range self.hsetIndexes {
		indexList.HsetIndexes = append(indexList.HsetIndexes, v.HsetIndexInfo)
	}
	return indexList.Marshal()
}

func (self *TableIndexContainer) unmarshalHsetIndexes(table []byte, data []byte) error {
	var indexList HsetIndexList
	err := indexList.Unmarshal(data)
	if err != nil {
		return err
	}
	self.hsetIndexes = make(map[string]*HsetIndex)
	for _, v := range indexList.HsetIndexes {
		var hi HsetIndex
		hi.HsetIndexInfo = v
		hi.Table = table
		self.hsetIndexes[string(v.IndexField)] = &hi
	}
	dbLog.Infof("load hash index: %v", indexList.String())
	return nil
}

type IndexMgr struct {
	sync.RWMutex
	tableIndexes   map[string]*TableIndexContainer
	closeChan      chan struct{}
	indexBuildChan chan int
	wg             sync.WaitGroup
}

func NewIndexMgr() *IndexMgr {
	return &IndexMgr{
		tableIndexes:   make(map[string]*TableIndexContainer),
		indexBuildChan: make(chan int, 10),
	}
}

func (self *IndexMgr) LoadIndexes(db *RockDB) error {
	dbLog.Infof("begin loading indexes...")
	defer dbLog.Infof("finish load indexes.")
	tables := db.GetHsetIndexTables()
	for t := range tables {
		d, err := db.GetTableHsetIndexValue(t)
		if err != nil {
			dbLog.Infof("get table %v hset index failed: %v", string(t), err)
			continue
		}
		if d == nil {
			dbLog.Infof("get table %v hset index empty", string(t))
			continue
		}
		indexes := NewIndexContainer()
		err = indexes.unmarshalHsetIndexes(t, d)
		if err != nil {
			dbLog.Infof("unmarshal table %v hset indexes failed: %v", string(t), err)
			return err
		}
		dbLog.Infof("table %v load %v hash indexes", string(t), len(indexes.hsetIndexes))
		self.Lock()
		self.tableIndexes[string(t)] = indexes
		self.Unlock()
	}

	self.Lock()
	if self.closeChan != nil {
		select {
		case <-self.closeChan:
		default:
			close(self.closeChan)
		}
	}
	self.closeChan = make(chan struct{})
	self.wg.Add(1)
	go func(stopC chan struct{}) {
		defer self.wg.Done()
		self.buildIndexes(db, stopC)
	}(self.closeChan)
	self.Unlock()
	select {
	case self.indexBuildChan <- 1:
	default:
	}
	return nil
}

func (self *IndexMgr) Close() {
	dbLog.Infof("closing index manager")
	self.Lock()
	self.tableIndexes = make(map[string]*TableIndexContainer)
	if self.closeChan != nil {
		select {
		case <-self.closeChan:
		default:
			close(self.closeChan)
		}
	}
	self.Unlock()
	self.wg.Wait()
	dbLog.Infof("index manager closed")
}

func (self *IndexMgr) AddHsetIndex(db *RockDB, hindex *HsetIndex) error {
	self.Lock()
	indexes, ok := self.tableIndexes[string(hindex.Table)]
	if !ok {
		indexes = NewIndexContainer()
		self.tableIndexes[string(hindex.Table)] = indexes
	}
	self.Unlock()
	indexes.Lock()
	defer indexes.Unlock()
	_, ok = indexes.hsetIndexes[string(hindex.IndexField)]
	if ok {
		return ErrIndexExist
	}
	hindex.State = InitIndex
	indexes.hsetIndexes[string(hindex.IndexField)] = hindex
	d, err := indexes.marshalHsetIndexes()
	if err != nil {
		delete(indexes.hsetIndexes, string(hindex.IndexField))
		return err
	}
	err = db.SetTableHsetIndexValue(hindex.Table, d)
	if err != nil {
		delete(indexes.hsetIndexes, string(hindex.IndexField))
		return err
	}
	return err
}

func (self *IndexMgr) UpdateHsetIndexState(db *RockDB, table string, field string, state IndexState) error {
	self.RLock()
	isClosed := self.closeChan == nil
	indexes, ok := self.tableIndexes[table]
	self.RUnlock()
	if !ok {
		return ErrIndexNotExist
	}
	if isClosed {
		return ErrIndexClosed
	}

	indexes.Lock()
	defer indexes.Unlock()
	index, ok := indexes.hsetIndexes[field]
	if !ok {
		return ErrIndexNotExist
	}
	if index.State == state {
		return nil
	}
	oldState := index.State
	index.State = state
	d, err := indexes.marshalHsetIndexes()
	if err != nil {
		index.State = oldState
		return err
	}
	err = db.SetTableHsetIndexValue([]byte(table), d)
	if err != nil {
		index.State = oldState
		return err
	}
	if index.State == DeletedIndex {
		self.wg.Add(1)
		go func() {
			defer self.wg.Done()
			err := index.cleanAll(db, self.closeChan)
			if err != nil {
				dbLog.Infof("failed to clean index: %v", err)
			} else {
				self.deleteHsetIndex(db, string(index.Table), string(index.IndexField))
			}
		}()
	} else if index.State == BuildingIndex {
		select {
		case self.indexBuildChan <- 1:
		default:
		}
	}

	return nil
}

// ensure mark index as deleted, and clean in background before delete the index
func (self *IndexMgr) deleteHsetIndex(db *RockDB, table string, field string) error {
	self.Lock()
	indexes, ok := self.tableIndexes[table]
	self.Unlock()
	if !ok {
		return ErrIndexNotExist
	}

	indexes.Lock()
	defer indexes.Unlock()
	hindex, ok := indexes.hsetIndexes[field]
	if !ok {
		return ErrIndexNotExist
	}
	if hindex.State != DeletedIndex {
		return ErrIndexDeleteNotInDeleted
	}
	delete(indexes.hsetIndexes, field)
	d, err := indexes.marshalHsetIndexes()
	if err != nil {
		return err
	}
	err = db.SetTableHsetIndexValue([]byte(table), d)
	if err != nil {
		return err
	}
	return nil
}

func (self *IndexMgr) GetHsetIndex(table string, field string) (*HsetIndex, error) {
	self.RLock()
	indexes, ok := self.tableIndexes[table]
	self.RUnlock()
	if !ok {
		return nil, ErrIndexNotExist
	}

	indexes.Lock()
	defer indexes.Unlock()
	index, ok := indexes.hsetIndexes[field]
	if !ok {
		return nil, ErrIndexNotExist
	}

	return index, nil
}

func (self *IndexMgr) buildIndexes(db *RockDB, stopChan chan struct{}) {
	for {
		select {
		case <-self.indexBuildChan:
			self.dobuildIndexes(db, stopChan)
		case <-stopChan:
			return
		}
	}
}

func (self *IndexMgr) dobuildIndexes(db *RockDB, stopChan chan struct{}) {
	tmpHsetIndexes := make([]*HsetIndex, 0)
	var buildWg sync.WaitGroup
	self.Lock()
	for table, v := range self.tableIndexes {
		for _, hindex := range v.hsetIndexes {
			if hindex.State == BuildingIndex {
				tmpHsetIndexes = append(tmpHsetIndexes, hindex)
			}
		}
		fields := make([][]byte, 0)
		for _, hindex := range tmpHsetIndexes {
			fields = append(fields, hindex.IndexField)
		}

		buildWg.Add(1)
		go func(buildTable string, t *TableIndexContainer) {
			defer buildWg.Done()
			dbLog.Infof("begin rebuild index for table %v", buildTable)
			for _, f := range fields {
				dbLog.Infof("begin rebuild index for field: %s", string(f))
			}
			cursor := []byte(buildTable + ":")
			for {
				done, err := func() (bool, error) {
					t.Lock()
					defer t.Unlock()
					select {
					case <-stopChan:
						dbLog.Infof("rebuild index for table %v stopped", buildTable)
						return true, ErrIndexClosed
					default:
					}

					pkList, err := db.Scan(common.HASH, cursor, buildIndexBlock, "")
					if err != nil {
						dbLog.Infof("rebuild index for table %v error %v", buildTable, err)
						return true, err
					}
					wb := gorocksdb.NewWriteBatch()
					for _, pk := range pkList {
						if !bytes.HasPrefix(pk, []byte(buildTable)) {
							dbLog.Infof("rebuild index for table %v end at: %v", buildTable, string(cursor))
							cursor = nil
							break
						}
						values, err := db.HMget(pk, fields...)
						if err != nil {
							dbLog.Infof("rebuild index for table %v error %v ", buildTable, err)
							return true, err
						}
						err = db.hsetIndexAddFieldRecs(pk, fields, values, wb)
						if err != nil {
							dbLog.Infof("rebuild index for table %v error %v ", buildTable, err)
							return true, err
						}
						cursor = pk
					}
					db.eng.Write(db.defaultWriteOpts, wb)
					wb.Destroy()
					if len(cursor) == 0 {
						return true, nil
					} else {
						dbLog.Infof("rebuild index for table %v cursor: %s", buildTable, string(cursor))
					}
					return false, nil
				}()
				if done {
					if err != nil {
					} else {
						dbLog.Infof("finish rebuild index for table %v", buildTable)
						// TODO: change index state to build done
					}
					break
				}
			}
		}(table, v)
	}
	self.Unlock()

	buildWg.Wait()
}