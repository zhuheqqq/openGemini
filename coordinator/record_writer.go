/*
Copyright 2023 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/arrow/go/arrow/array"
	"github.com/openGemini/openGemini/lib/bufferpool"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/logger"
	"github.com/openGemini/openGemini/lib/metaclient"
	"github.com/openGemini/openGemini/lib/record"
	"github.com/openGemini/openGemini/lib/statisticsPusher/statistics"
	"github.com/openGemini/openGemini/lib/util"
	"github.com/openGemini/openGemini/open_src/influx/influxql"
	"github.com/openGemini/openGemini/open_src/influx/meta"
	"github.com/openGemini/openGemini/open_src/influx/meta/proto"
	"go.uber.org/zap"
)

type RWMetaClient interface {
	Database(name string) (di *meta.DatabaseInfo, err error)
	RetentionPolicy(database, policy string) (*meta.RetentionPolicyInfo, error)
	CreateShardGroup(database, policy string, timestamp time.Time, engineType config.EngineType) (*meta.ShardGroupInfo, error)
	DBPtView(database string) (meta.DBPtInfos, error)
	Measurement(database string, rpName string, mstName string) (*meta.MeasurementInfo, error)
	UpdateSchema(database string, retentionPolicy string, mst string, fieldToCreate []*proto.FieldSchema) error
	CreateMeasurement(database string, retentionPolicy string, mst string, shardKey *meta.ShardKeyInfo, indexR *influxql.IndexRelation, engineType config.EngineType,
		colStoreInfo *meta.ColStoreInfo, schemaInfo []*proto.FieldSchema, options *meta.Options) (*meta.MeasurementInfo, error)
	GetShardInfoByTime(database, retentionPolicy string, t time.Time, ptIdx int, nodeId uint64, engineType config.EngineType) (*meta.ShardInfo, error)
}

// RecMsg data structure of the message of the record.
type RecMsg struct {
	Database        string
	RetentionPolicy string
	Measurement     string
	Rec             interface{}
}

// RecordWriter handles writes the local data node.
type RecordWriter struct {
	ptNum            int // ptNum is the number of partition on the current node
	recMsgChFactor   int // recMsgChFactor based on the rule of thumb, increase the capacity of ch and reduce block.
	nodeId           uint64
	MetaClient       RWMetaClient
	errs             *errno.Errs
	logger           *logger.Logger
	wg               sync.WaitGroup
	ctx              context.Context
	cancel           context.CancelFunc
	timeout          time.Duration
	recMsgCh         chan *RecMsg
	recWriterHelpers []*recordWriterHelper

	StorageEngine interface {
		WriteRec(db, rp, mst string, ptId uint32, shardID uint64, rec *record.Record, binaryRec []byte) error
	}
}

func NewRecordWriter(timeout time.Duration, ptNum, recMsgChFactor int) *RecordWriter {
	rw := &RecordWriter{
		timeout:        timeout,
		ptNum:          ptNum,
		wg:             sync.WaitGroup{},
		errs:           errno.NewErrs(),
		logger:         logger.NewLogger(errno.ModuleCoordinator),
		recMsgChFactor: recMsgChFactor,
	}
	rw.ctx, rw.cancel = context.WithCancel(context.Background())
	rw.errs.Init(rw.ptNum, rw.cancel)
	return rw
}

func (w *RecordWriter) RetryWriteRecord(database, retentionPolicy, measurement string, rec array.Record) error {
	w.recMsgCh <- &RecMsg{
		Database:        database,
		RetentionPolicy: retentionPolicy,
		Measurement:     measurement,
		Rec:             rec,
	}
	return nil
}

func (w *RecordWriter) RetryWriteLogRecord(database, retentionPolicy, measurement string, rec *record.Record) error {
	w.recMsgCh <- &RecMsg{
		Database:        database,
		RetentionPolicy: retentionPolicy,
		Measurement:     measurement,
		Rec:             rec,
	}
	return nil
}

func (w *RecordWriter) Open() error {
	w.logger = logger.NewLogger(errno.ModuleCoordinator)
	w.nodeId = metaclient.DefaultMetaClient.NodeID()
	w.logger.Info(fmt.Sprintf("init nodeId %d", w.nodeId))

	ptNum := w.ptNum
	w.wg.Add(ptNum)
	w.recMsgCh = make(chan *RecMsg, ptNum*w.recMsgChFactor)
	w.recWriterHelpers = make([]*recordWriterHelper, ptNum)
	for ptIdx := 0; ptIdx < ptNum; ptIdx++ {
		w.recWriterHelpers[ptIdx] = newRecordWriterHelper(w.MetaClient, w.nodeId)
		go func(idx int) {
			w.consume(idx)
		}(ptIdx)
	}
	go w.monitoring()
	return nil
}

func (w *RecordWriter) monitoring() {
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
			if err := w.errs.Err(); err != nil {
				w.logger.Error("record writer monitoring", zap.Error(errno.NewError(errno.RecordWriterFatalErr)))
				if err = w.Close(); err != nil {
					w.logger.Error("record writer closed", zap.Error(err))
				}
				return
			}
		}
	}
}

func (w *RecordWriter) Close() error {
	w.release()
	w.errs.Clean()
	w.wg.Wait()
	return nil
}

func (w *RecordWriter) release() {
	w.cancel()
	close(w.recMsgCh)
	w.recWriterHelpers = w.recWriterHelpers[:0]
}

func (w *RecordWriter) consume(ptIdx int) {
	defer w.wg.Done()
	for {
		select {
		case msg, ok := <-w.recMsgCh:
			if !ok {
				return
			}
			w.processRecord(msg, ptIdx)
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *RecordWriter) processRecord(msg *RecMsg, ptIdx int) {
	var writeErr error
	var rowNums int64
	defer func() {
		if err := recover(); err != nil {
			w.logger.Error("processRecord panic", zap.String("db", msg.Database), zap.String("rp", msg.RetentionPolicy), zap.String("mst", msg.Measurement),
				zap.String("record writer raise stack:", string(debug.Stack())),
				zap.Error(errno.NewError(errno.RecoverPanic, err)))
			w.errs.Dispatch(errno.NewError(errno.RecoverPanic, err))
		}
		if writeErr != nil && !IsKeepWritingErr(writeErr) {
			w.logger.Error("processRecord err", zap.String("db", msg.Database), zap.String("rp", msg.RetentionPolicy), zap.String("mst", msg.Measurement), zap.Error(writeErr))
		}
		switch m := msg.Rec.(type) {
		case array.Record:
			m.Release() // The memory is released. The value of reference counting is decreased by 1.
		case *record.Record:
			m.ResetForReuse()
		default:
			break
		}
	}()

	start := time.Now()
	switch m := msg.Rec.(type) {
	case array.Record:
		writeErr = w.writeRecord(msg.Database, msg.RetentionPolicy, msg.Measurement, m, ptIdx)
		rowNums = m.NumRows()
	case *record.Record:
		writeErr = w.writeLogRecord(msg.Database, msg.RetentionPolicy, msg.Measurement, m, ptIdx)
		rowNums = int64(m.RowNums())
	default:
		break
	}
	if writeErr != nil {
		w.recWriterHelpers[ptIdx].reset()
		return
	}
	atomic.AddInt64(&statistics.HandlerStat.PointsWrittenOK, rowNums)
	atomic.AddInt64(&statistics.HandlerStat.WriteStoresDuration, time.Since(start).Nanoseconds())
}

func (w *RecordWriter) writeRecord(db, rp, mst string, rec array.Record, ptIdx int) error {
	colNum, rowNum := rec.NumCols(), rec.NumRows()
	if colNum == 0 || rowNum == 0 {
		return nil
	}

	ctx := getWriteRecCtx()
	defer putWriteRecCtx(ctx)

	wh := w.recWriterHelpers[ptIdx]
	err := ctx.checkDBRP(db, rp, wh.metaClient)
	if err != nil {
		w.logger.Error("checkDBRP err", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
		return err
	}

	if rp == "" {
		rp = ctx.db.DefaultRetentionPolicy
	}

	originName := mst
	ctx.ms, err = wh.createMeasurement(db, rp, mst)
	if err != nil {
		w.logger.Error("invalid measurement", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(errno.NewError(errno.InvalidMeasurement)))
		return err
	}
	mst = ctx.ms.Name

	startTime, endTime, r, err := wh.checkAndUpdateSchema(db, rp, mst, originName, rec)
	if err != nil {
		wh.sameSchema = false
		w.logger.Error("checkSchema err", zap.String("db", db), zap.String("rp", rp), zap.Error(err))
		return err
	}

	err = record.ArrowRecordToNativeRecord(rec, r)
	if err != nil {
		w.logger.Error("ArrowRecordToNativeRecord failed", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
		return err
	}

	sgis, err := wh.createShardGroupsByTimeRange(db, rp, time.Unix(0, startTime), time.Unix(0, endTime), ctx.ms.EngineType)
	if err != nil {
		w.logger.Error("create shard group failed", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
		return err
	}
	atomic.AddInt64(&statistics.HandlerStat.FieldsWritten, rec.NumRows()*rec.NumCols())
	return w.splitAndWriteByShard(sgis, db, rp, mst, r, ptIdx, ctx.ms.EngineType)
}

func (w *RecordWriter) writeLogRecord(db, rp, mst string, rec *record.Record, ptIdx int) error {
	colNum, rowNum := rec.ColNums(), rec.RowNums()
	if colNum == 0 || rowNum == 0 {
		return nil
	}

	ctx := getWriteRecCtx()
	defer putWriteRecCtx(ctx)

	wh := w.recWriterHelpers[ptIdx]
	err := ctx.checkDBRP(db, rp, wh.metaClient)
	if err != nil {
		w.logger.Error("checkDBRP err", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
		return err
	}

	if rp == "" {
		rp = ctx.db.DefaultRetentionPolicy
	}

	ctx.ms, err = wh.createMeasurement(db, rp, mst)
	if err != nil {
		w.logger.Error("invalid measurement", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(errno.NewError(errno.InvalidMeasurement)))
		return err
	}
	mst = ctx.ms.Name

	timeCol := &rec.ColVals[rec.ColNums()-1]
	startTime, endTime := timeCol.IntegerValues()[0], timeCol.IntegerValues()[timeCol.Len-1]
	sgis, err := wh.createShardGroupsByTimeRange(db, rp, time.Unix(0, startTime), time.Unix(0, endTime), ctx.ms.EngineType)
	if err != nil {
		w.logger.Error("create shard group failed", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
		return err
	}
	atomic.AddInt64(&statistics.HandlerStat.FieldsWritten, int64(rec.RowNums()*rec.ColNums()))
	return w.splitAndWriteByShard(sgis, db, rp, mst, rec, ptIdx, ctx.ms.EngineType)
}

func (w *RecordWriter) splitAndWriteByShard(sgis []*meta.ShardGroupInfo, db, rp, mst string, rec *record.Record, ptIdx int, engineType config.EngineType) error {
	start := 0
	var subRec *record.Record
	var err error
	for i := range sgis {
		interval := SearchLowerBoundOfRec(rec, sgis[i], start)
		if interval == NotInShardDuration {
			err = errno.NewError(errno.ArrowFlightGetShardGroupErr, sgis[i].StartTime, rec.Time(start))
			w.logger.Warn("SearchLowerBoundOfRec failed", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
			continue
		}
		end := start + interval
		if start == 0 && end == rec.RowNums() {
			subRec = rec
		} else {
			subRec = record.NewRecord(rec.Schema, false)
			subRec.SliceFromRecord(rec, start, end)
		}
		shard, err := w.recWriterHelpers[ptIdx].GetShardByTime(sgis[i], db, rp, time.Unix(0, subRec.Time(0)), ptIdx, engineType)
		if err != nil {
			w.logger.Error("GetShardByTime failed", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
			return err
		}
		err = w.writeRecordToShard(shard, db, rp, mst, subRec)
		if err != nil {
			w.logger.Error("writeRecordToShard failed", zap.String("db", db), zap.String("rp", rp), zap.String("mst", mst), zap.Error(err))
			return err
		}
		start = end
	}
	return err
}

func (w *RecordWriter) writeRecordToShard(shard *meta.ShardInfo, database, retentionPolicy, measurement string, rec *record.Record) error {
	var err error
	var toContinue bool
	start := time.Now()

	pBuf := bufferpool.GetPoints()
	defer func() {
		bufferpool.PutPoints(pBuf)
	}()

	pBuf, err = MarshalWithMeasurements(pBuf[:0], measurement, rec)
	if err != nil {
		w.logger.Error(fmt.Sprintf("record marshal failed. db: %s, rp: %s, mst: %s", database, retentionPolicy, measurement), zap.Error(err))
		return err
	}
	atomic.AddInt64(&statistics.HandlerStat.WriteRequestBytesReceived, int64(len(pBuf)))
	atomic.AddInt64(&statistics.HandlerStat.WriteRequestBytesIn, int64(len(pBuf)))

	for {
		// retry timeout
		if time.Since(start).Nanoseconds() >= w.timeout.Nanoseconds() {
			w.logger.Error(fmt.Sprintf("[coordinator] write rows timeout. db: %s, rp: %s, mst: %s", database, retentionPolicy, measurement), zap.Uint32s("ptIds", shard.Owners), zap.Error(err))
			return err
		}

		toContinue, err = w.writeShard(shard, database, retentionPolicy, measurement, rec, pBuf)
		if toContinue {
			continue
		}
		return err
	}
}

func (w *RecordWriter) writeShard(shard *meta.ShardInfo, database, retentionPolicy, measurement string, rec *record.Record, pBuf []byte) (bool, error) {
	var err error
	for _, ptId := range shard.Owners {
		err = w.StorageEngine.WriteRec(database, retentionPolicy, measurement, ptId, shard.ID, rec, pBuf)
		if err != nil && IsRetryErrorForPtView(err) {
			// maybe dbPt route to new node, retry get the right nodeID
			w.logger.Error(fmt.Sprintf("[coordinator] retry write rows. db: %s, rp: %s, mst: %s", database, retentionPolicy, measurement), zap.Uint32("pt", ptId), zap.Error(err))

			// The retry interval is added to avoid excessive error logs
			time.Sleep(100 * time.Millisecond)
			return true, err
		}
		if err != nil {
			w.logger.Error(fmt.Sprintf("[coordinator] write shard. db: %s, rp: %s, mst: %s", database, retentionPolicy, measurement), zap.Uint32("pt", ptId), zap.Error(err))
			return false, err
		}
	}
	return false, nil
}

func MarshalWithMeasurements(buf []byte, mst string, rec *record.Record) ([]byte, error) {
	name := util.Str2bytes(mst)
	if len(name) == 0 {
		return nil, errors.New("record must have a measurement name")
	}
	buf = append(buf, uint8(len(name)))
	buf = append(buf, name...)

	return rec.Marshal(buf)
}

func UnmarshalWithMeasurements(buf []byte, rec *record.Record) (string, error) {
	if len(buf) < 1 {
		return "", errors.New("too small bytes for record binary")
	}

	mLen := int(buf[0])
	buf = buf[1:]

	name := util.Bytes2str(buf[:mLen])
	buf = buf[mLen:]
	return name, rec.Unmarshal(buf)
}

func IsKeepWritingErr(err error) bool {
	return errno.Equal(err, errno.DatabaseNotFound) ||
		errno.Equal(err, errno.ErrMeasurementNotFound) ||
		errno.Equal(err, errno.TypeAssertFail) ||
		errno.Equal(err, errno.ArrowFlightGetShardGroupErr) ||
		errno.Equal(err, errno.ShardNotFound) ||
		errno.Equal(err, errno.ColumnStoreColNumErr) ||
		errno.Equal(err, errno.ColumnStoreSchemaNullErr) ||
		errno.Equal(err, errno.ColumnStorePrimaryKeyNullErr) ||
		errno.Equal(err, errno.ColumnStorePrimaryKeyLackErr) ||
		errno.Equal(err, errno.ArrowRecordTimeFieldErr) ||
		errno.Equal(err, errno.ColumnStoreFieldNameErr) ||
		errno.Equal(err, errno.ColumnStoreFieldTypeErr) ||
		errno.Equal(err, errno.BucketLacks) ||
		errno.Equal(err, errno.PtNotFound)
}
