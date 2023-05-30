// Copyright 2018 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package data

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
	"golang.org/x/time/rate"
)

type InsertExtentKeyFunc func(ctx context.Context, inode uint64, key proto.ExtentKey, isPreExtent bool) error
type GetExtentsFunc func(ctx context.Context, inode uint64) (uint64, uint64, []proto.ExtentKey, error)
type TruncateFunc func(ctx context.Context, inode, oldSize, size uint64) error
type EvictIcacheFunc func(ctx context.Context, inode uint64)
type InodeMergeExtentsFunc func(ctx context.Context, inode uint64, oldEks []proto.ExtentKey, newEk []proto.ExtentKey) error

const (
	MaxMountRetryLimit = 5
	MountRetryInterval = time.Second * 5

	defaultReadLimitRate   = rate.Inf
	defaultReadLimitBurst  = 128
	defaultWriteLimitRate  = rate.Inf
	defaultWriteLimitBurst = 128
	updateConfigTicket     = 1 * time.Minute

	defaultMaxAlignSize = 128 * 1024
)

var (
	// global object pools for memory optimization
	openRequestPool    *sync.Pool
	writeRequestPool   *sync.Pool
	flushRequestPool   *sync.Pool
	releaseRequestPool *sync.Pool
	truncRequestPool   *sync.Pool
	evictRequestPool   *sync.Pool
)

func init() {
	// init object pools
	openRequestPool = &sync.Pool{New: func() interface{} {
		return &OpenRequest{}
	}}
	writeRequestPool = &sync.Pool{New: func() interface{} {
		return &WriteRequest{}
	}}
	flushRequestPool = &sync.Pool{New: func() interface{} {
		return &FlushRequest{}
	}}
	releaseRequestPool = &sync.Pool{New: func() interface{} {
		return &ReleaseRequest{}
	}}
	truncRequestPool = &sync.Pool{New: func() interface{} {
		return &TruncRequest{}
	}}
	evictRequestPool = &sync.Pool{New: func() interface{} {
		return &EvictRequest{}
	}}
}

type ExtentConfig struct {
	Volume                   string
	Masters                  []string
	FollowerRead             bool
	NearRead                 bool
	ReadRate                 int64
	WriteRate                int64
	AlignSize                int64
	TinySize                 int
	ExtentSize               int
	MaxExtentNumPerAlignArea int64
	ForceAlignMerge          bool
	AutoFlush                bool
	OnInsertExtentKey        InsertExtentKeyFunc
	OnGetExtents             GetExtentsFunc
	OnTruncate               TruncateFunc
	OnEvictIcache            EvictIcacheFunc
	OnInodeMergeExtents      InodeMergeExtentsFunc
	ExtentMerge              bool
	MetaWrapper              *meta.MetaWrapper
}

// ExtentClient defines the struct of the extent client.
type ExtentClient struct {
	//streamers    map[uint64]*Streamer
	//streamerLock sync.Mutex
	streamerConcurrentMap ConcurrentStreamerMap

	originReadRate  int64
	originWriteRate int64
	readRate        uint64
	writeRate       uint64
	readLimiter     *rate.Limiter
	writeLimiter    *rate.Limiter
	masterClient    *masterSDK.MasterClient

	dataWrapper     *Wrapper
	metaWrapper     *meta.MetaWrapper
	insertExtentKey InsertExtentKeyFunc
	getExtents      GetExtentsFunc
	truncate        TruncateFunc
	evictIcache     EvictIcacheFunc //May be null, must check before using

	followerRead             bool
	alignSize                int64
	maxExtentNumPerAlignArea int64
	forceAlignMerge          bool

	tinySize   int
	extentSize int
	autoFlush  bool

	stopC chan struct{}
	wg    sync.WaitGroup

	extentMerge        bool
	extentMergeIno     []uint64
	extentMergeChan    chan struct{}
	ExtentMergeSleepMs uint64
}

const (
	NoUseTinyExtent = -1
)

// NewExtentClient returns a new extent client.
func NewExtentClient(config *ExtentConfig, dataState *DataState) (client *ExtentClient, err error) {
	client = new(ExtentClient)

	if dataState != nil {
		client.dataWrapper = RebuildDataPartitionWrapper(config.Volume, config.Masters, dataState)
	} else {
		limit := MaxMountRetryLimit
	retry:
		client.dataWrapper, err = NewDataPartitionWrapper(config.Volume, config.Masters)
		if err != nil {
			if limit <= 0 {
				return nil, errors.Trace(err, "Init data wrapper failed!")
			} else {
				limit--
				time.Sleep(MountRetryInterval)
				goto retry
			}
		}
	}
	client.metaWrapper = config.MetaWrapper

	client.streamerConcurrentMap = InitConcurrentStreamerMap()
	client.insertExtentKey = config.OnInsertExtentKey
	client.getExtents = config.OnGetExtents
	client.truncate = config.OnTruncate
	client.evictIcache = config.OnEvictIcache
	client.dataWrapper.InitFollowerRead(config.FollowerRead)
	client.dataWrapper.SetNearRead(config.NearRead)
	client.tinySize = config.TinySize
	if client.tinySize == 0 {
		client.tinySize = unit.DefaultTinySizeLimit
	}
	client.SetExtentSize(config.ExtentSize)
	client.autoFlush = config.AutoFlush

	if client.tinySize == NoUseTinyExtent {
		client.tinySize = 0
	}
	var readLimit, writeLimit rate.Limit
	if config.ReadRate <= 0 {
		readLimit = defaultReadLimitRate
	} else {
		readLimit = rate.Limit(config.ReadRate)
		client.originReadRate = config.ReadRate
	}
	if config.WriteRate <= 0 {
		writeLimit = defaultWriteLimitRate
	} else {
		writeLimit = rate.Limit(config.WriteRate)
		client.originWriteRate = config.WriteRate
	}

	client.readLimiter = rate.NewLimiter(readLimit, defaultReadLimitBurst)
	client.writeLimiter = rate.NewLimiter(writeLimit, defaultWriteLimitBurst)
	client.masterClient = masterSDK.NewMasterClient(config.Masters, false)
	client.wg.Add(1)
	go client.startUpdateConfig()

	client.alignSize = config.AlignSize
	if client.alignSize > defaultMaxAlignSize {
		log.LogWarnf("config alignSize(%v) is too max, set it to default max value(%v).", client.alignSize,
			defaultMaxAlignSize)
		client.alignSize = defaultMaxAlignSize
	}
	client.maxExtentNumPerAlignArea = config.MaxExtentNumPerAlignArea
	client.forceAlignMerge = config.ForceAlignMerge
	client.stopC = make(chan struct{})

	client.extentMerge = config.ExtentMerge
	if client.extentMerge {
		client.extentMergeChan = make(chan struct{})
		client.wg.Add(1)
		go client.BackgroundExtentMerge()
	}
	return
}

func RebuildExtentClient(config *ExtentConfig, dataState *DataState) (client *ExtentClient) {
	client, _ = NewExtentClient(config, dataState)
	return
}

func (client *ExtentClient) SaveDataState() *DataState {
	return client.dataWrapper.saveDataState()
}

// Open request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) OpenStream(inode uint64, appendWriteBuffer bool, readAhead bool) error {
	streamerMapSeg := client.streamerConcurrentMap.GetMapSegment(inode)
	streamerMapSeg.Lock()
	s, ok := streamerMapSeg.streamers[inode]
	if !ok {
		s = NewStreamer(client, inode, streamerMapSeg, appendWriteBuffer, readAhead)
		streamerMapSeg.streamers[inode] = s
	}
	return s.IssueOpenRequest()
}

// Release request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) CloseStream(ctx context.Context, inode uint64) error {
	streamerMapSeg := client.streamerConcurrentMap.GetMapSegment(inode)
	streamerMapSeg.Lock()
	s, ok := streamerMapSeg.streamers[inode]
	if !ok {
		streamerMapSeg.Unlock()
		return nil
	}
	return s.IssueReleaseRequest(ctx)
}

func (client *ExtentClient) MustCloseStream(ctx context.Context, inode uint64) error {
	streamerMapSeg := client.streamerConcurrentMap.GetMapSegment(inode)
	streamerMapSeg.Lock()
	s, ok := streamerMapSeg.streamers[inode]
	if !ok {
		streamerMapSeg.Unlock()
		return nil
	}
	return s.IssueMustReleaseRequest(ctx)
}

// Evict request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) EvictStream(ctx context.Context, inode uint64) error {
	streamerMapSeg := client.streamerConcurrentMap.GetMapSegment(inode)
	streamerMapSeg.Lock()
	s, ok := streamerMapSeg.streamers[inode]
	if !ok {
		streamerMapSeg.Unlock()
		return nil
	}
	err := s.IssueEvictRequest(ctx)
	if err != nil {
		return err
	}

	s.done <- struct{}{}
	s.wg.Wait()
	return nil
}

// RefreshExtentsCache refreshes the extent cache.
func (client *ExtentClient) RefreshExtentsCache(ctx context.Context, inode uint64) error {
	s := client.GetStreamer(inode)
	if s == nil {
		return fmt.Errorf("stream is not opened yet, inode:%v", inode)
		// return nil
	}
	return s.GetExtents(ctx)
}

// FileSize returns the file size.
func (client *ExtentClient) FileSize(inode uint64) (size uint64, gen uint64, valid bool) {
	s := client.GetStreamer(inode)
	if s == nil {
		return
	}
	valid = true
	size, gen = s.extents.Size()
	return
}

// SetFileSize set the file size.
//func (client *ExtentClient) SetFileSize(inode uint64, size int) {
//	s := client.GetStreamer(inode)
//	if s != nil {
//		log.LogDebugf("SetFileSize: ino(%v) size(%v)", inode, size)
//		s.extents.SetSize(uint64(size), true)
//	}
//}

// Write writes the data.
func (client *ExtentClient) Write(ctx context.Context, inode uint64, offset uint64, data []byte, direct bool, overWriteBuffer bool) (write int, isROW bool, err error) {
	if client.dataWrapper.VolNotExists() {
		return 0, false, proto.ErrVolNotExists
	}

	s := client.GetStreamer(inode)
	if s == nil {
		prefix := fmt.Sprintf("Write{ino(%v)offset(%v)size(%v)}", inode, offset, len(data))
		return 0, false, fmt.Errorf("Prefix(%v): stream is not opened yet", prefix)
	}
	s.once.Do(func() {
		s.GetExtents(ctx)
	})
	if !s.extents.initialized {
		return 0, false, proto.ErrGetExtentsFailed
	}

	if overWriteBuffer {
		requests, _ := s.extents.PrepareRequests(offset, len(data), data)
		hasAppendWrite := false
		for _, req := range requests {
			if req.ExtentKey == nil {
				hasAppendWrite = true
				break
			}
		}
		if hasAppendWrite {
			write, isROW, err = s.IssueWriteRequest(ctx, offset, data, direct, overWriteBuffer)
		} else {
			for _, req := range requests {
				write += s.appendOverWriteReq(req, direct)
			}
		}
	} else {
		write, isROW, err = s.IssueWriteRequest(ctx, offset, data, direct, overWriteBuffer)
	}

	return
}

func (client *ExtentClient) SyncWrite(ctx context.Context, inode uint64, offset uint64, data []byte) (dp *DataPartition, write int, newEk *proto.ExtentKey, err error) {
	if client.dataWrapper.VolNotExists() {
		return nil, 0, nil, proto.ErrVolNotExists
	}

	prefix := fmt.Sprintf("SyncWrite{ino(%v)offset(%v)size(%v)}", inode, offset, len(data))
	s := client.GetStreamer(inode)
	if s == nil {
		return nil, 0, nil, fmt.Errorf("Prefix(%v): stream is not opened yet", prefix)
	}

	oriReq := &ExtentRequest{FileOffset: offset, Size: len(data), Data: data}
	var exID int
	dp, exID, write, err = s.writeToNewExtent(ctx, oriReq, true)
	if err != nil {
		return
	}
	newEk = &proto.ExtentKey{
		PartitionId:  dp.PartitionID,
		ExtentId:     uint64(exID),
		ExtentOffset: 0,
		FileOffset:   uint64(offset),
		Size:         uint32(len(data)),
	}
	return
}

func (client *ExtentClient) SyncWriteToSpecificExtent(ctx context.Context, dp *DataPartition, inode uint64, fileOffset uint64, extentOffset int, data []byte, extID int) (total int, err error) {
	if client.dataWrapper.VolNotExists() {
		return 0, proto.ErrVolNotExists
	}

	prefix := fmt.Sprintf("SyncWriteToExtent{ino(%v)fileOffset(%v)extentOffset(%v)size(%v)}", inode, fileOffset, extentOffset, len(data))
	s := client.GetStreamer(inode)
	if s == nil {
		return 0, fmt.Errorf("prefix(%v): stream is not opened yet", prefix)
	}

	oriReq := &ExtentRequest{FileOffset: fileOffset, Size: len(data), Data: data}
	total, err = s.writeToSpecificExtent(ctx, oriReq, extID, extentOffset, dp, true)
	if err != nil {
		return
	}
	return
}

func (client *ExtentClient) Truncate(ctx context.Context, inode uint64, size uint64) error {
	if client.dataWrapper.VolNotExists() {
		return proto.ErrVolNotExists
	}

	prefix := fmt.Sprintf("Truncate{ino(%v)size(%v)}", inode, size)
	s := client.GetStreamer(inode)
	if s == nil {
		return fmt.Errorf("Prefix(%v): stream is not opened yet", prefix)
	}

	// GetExtents if has not been called, to prevent file old size check failure.
	s.once.Do(func() {
		s.GetExtents(ctx)
	})
	if !s.extents.initialized {
		return proto.ErrGetExtentsFailed
	}

	err := s.IssueTruncRequest(ctx, size)
	if err != nil {
		err = errors.Trace(err, prefix)
		log.LogError(errors.Stack(err))
	}
	return err
}

func (client *ExtentClient) Flush(ctx context.Context, inode uint64) error {
	if client.dataWrapper.VolNotExists() {
		return proto.ErrVolNotExists
	}

	s := client.GetStreamer(inode)
	if s == nil {
		return fmt.Errorf("Flush: stream is not opened yet, ino(%v)", inode)
	}
	return s.IssueFlushRequest(ctx)
}

func (client *ExtentClient) Read(ctx context.Context, inode uint64, data []byte, offset uint64, size int) (read int, hasHole bool, err error) {
	if size == 0 {
		return
	}

	if client.dataWrapper.VolNotExists() {
		err = proto.ErrVolNotExists
		return
	}

	s := client.GetStreamer(inode)
	if s == nil {
		err = fmt.Errorf("Read: stream is not opened yet, ino(%v) offset(%v) size(%v)", inode, offset, size)
		return
	}

	s.once.Do(func() {
		s.GetExtents(ctx)
	})
	if !s.extents.initialized {
		err = proto.ErrGetExtentsFailed
		return
	}

	//err = s.IssueFlushRequest(ctx)
	//if err != nil {
	//	return
	//}

	// ROW in cross-region mode maybe insert a new ek
	s.UpdateExpiredExtentCache(ctx)

	read, hasHole, err = s.read(ctx, data, offset, size)
	if err != nil && strings.Contains(err.Error(), proto.ExtentNotFoundError.Error()) {
		if !s.extents.IsExpired(1) {
			return
		}

		err = s.IssueFlushRequest(ctx)
		if err != nil {
			return
		}
		if err = s.GetExtents(ctx); err != nil {
			return
		}
		read, hasHole, err = s.read(ctx, data, offset, size)
		log.LogWarnf("Retry read after refresh extent keys: ino(%v) offset(%v) size(%v) result size(%v) hasHole(%v) err(%v)",
			s.inode, offset, size, read, hasHole, err)
	}
	return
}

func (client *ExtentClient) ExtentMerge(ctx context.Context, inode uint64) (finish bool, err error) {
	s := client.GetStreamer(inode)
	if s == nil {
		log.LogErrorf("stream is not opened yet: inode(%v)", inode)
		return true, fmt.Errorf("stream is not opened yet")
	}
	s.once.Do(func() {
		s.GetExtents(ctx)
	})
	if !s.extents.initialized {
		return true, proto.ErrGetExtentsFailed
	}
	finish, err = s.IssueExtentMergeRequest(ctx)
	return
}

// GetStreamer returns the streamer.
func (client *ExtentClient) GetStreamer(inode uint64) *Streamer {
	streamerMapSeg := client.streamerConcurrentMap.GetMapSegment(inode)
	streamerMapSeg.RLock()
	defer streamerMapSeg.RUnlock()
	s, ok := streamerMapSeg.streamers[inode]
	if !ok {
		return nil
	}
	return s
}

func (client *ExtentClient) GetRate() string {
	return fmt.Sprintf("read: %v\nwrite: %v\n", getRate(client.readLimiter), getRate(client.writeLimiter))
}

func getRate(lim *rate.Limiter) string {
	val := int(lim.Limit())
	if val > 0 {
		return fmt.Sprintf("%v", val)
	}
	return "unlimited"
}

func (client *ExtentClient) SetReadRate(val int) string {
	client.originReadRate = int64(val)
	return setRate(client.readLimiter, val)
}

func (client *ExtentClient) SetWriteRate(val int) string {
	client.originWriteRate = int64(val)
	return setRate(client.writeLimiter, val)
}

func setRate(lim *rate.Limiter, val int) string {
	if val > 0 {
		lim.SetLimit(rate.Limit(val))
		return fmt.Sprintf("%v", val)
	}
	lim.SetLimit(rate.Inf)
	return "unlimited"
}

func (client *ExtentClient) startUpdateConfig() {
	defer client.wg.Done()
	for {
		err := client.startUpdateConfigWithRecover()
		if err == nil {
			break
		}
		log.LogErrorf("updateDataLimitConfig: err(%v) try next update", err)
	}
}

func (client *ExtentClient) startUpdateConfigWithRecover() (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("updateDataLimitConfig panic: err(%v) stack(%v)", r, string(debug.Stack()))
			msg := fmt.Sprintf("updateDataLimitConfig panic: err(%v)", r)
			handleUmpAlarm(client.dataWrapper.clusterName, client.dataWrapper.volName, "updateDataLimitConfig", msg)
			err = errors.New(msg)
		}
	}()
	ticker := time.NewTicker(updateConfigTicket)
	defer ticker.Stop()
	for {
		select {
		case <-client.stopC:
			return
		case <-ticker.C:
			client.updateConfig()
		}
	}
}

func (client *ExtentClient) updateConfig() {
	limitInfo, err := client.masterClient.AdminAPI().GetLimitInfo(client.dataWrapper.volName)
	if err != nil {
		log.LogWarnf("[updateConfig] %s", err.Error())
		return
	}
	// If rate from master is 0, then restore the client rate
	var (
		ok                  bool
		readRate, writeRate uint64
	)
	readRate, ok = limitInfo.ClientReadVolRateLimitMap[client.dataWrapper.volName]
	if !ok {
		readRate, ok = limitInfo.ClientReadVolRateLimitMap[""]
	}
	if (!ok || readRate == 0) && client.originReadRate > 0 {
		readRate = uint64(client.originReadRate)
	}
	client.readRate = readRate
	if readRate > 0 {
		client.readLimiter.SetLimit(rate.Limit(readRate))
	} else {
		client.readLimiter.SetLimit(rate.Limit(defaultReadLimitRate))
	}

	writeRate, ok = limitInfo.ClientWriteVolRateLimitMap[client.dataWrapper.volName]
	if !ok {
		writeRate, ok = limitInfo.ClientWriteVolRateLimitMap[""]
	}
	if (!ok || writeRate == 0) && client.originWriteRate > 0 {
		writeRate = uint64(client.originWriteRate)
	}
	client.writeRate = writeRate
	if writeRate > 0 {
		client.writeLimiter.SetLimit(rate.Limit(writeRate))
	} else {
		client.writeLimiter.SetLimit(rate.Limit(defaultWriteLimitRate))
	}

	if client.extentMerge {
		if len(client.extentMergeIno) == 0 && len(limitInfo.ExtentMergeIno[client.dataWrapper.volName]) > 0 {
			client.extentMergeChan <- struct{}{}
		}
		client.extentMergeIno = limitInfo.ExtentMergeIno[client.dataWrapper.volName]
		client.ExtentMergeSleepMs = limitInfo.ExtentMergeSleepMs
	}
}

func (client *ExtentClient) Close(ctx context.Context) error {
	close(client.stopC)
	client.wg.Wait()
	client.dataWrapper.Stop()
	// release streamers
	inodes := client.streamerConcurrentMap.Keys()
	for _, inode := range inodes {
		_ = client.Flush(ctx, inode)
		_ = client.MustCloseStream(ctx, inode)
		_ = client.EvictStream(ctx, inode)
	}
	return nil
}

func (client *ExtentClient) CloseConnPool() {
	if StreamConnPool != nil {
		StreamConnPool.Close()
		StreamConnPool = nil
	}
}

//func (c *ExtentClient) AlignSize() int {
//	return int(c.alignSize)
//}
//
//func (c *ExtentClient) MaxExtentNumPerAlignArea() int {
//	return int(c.maxExtentNumPerAlignArea)
//}
//
//func (c *ExtentClient) ForceAlignMerge() bool {
//	return c.forceAlignMerge
//}

func (c *ExtentClient) SetExtentSize(size int) {
	if size == 0 {
		c.extentSize = unit.ExtentSize
		return
	}
	if size > unit.ExtentSize {
		log.LogWarnf("too large extent size config %v, use default value %v", size, unit.ExtentSize)
		c.extentSize = unit.ExtentSize
		return
	}
	if size < unit.MinExtentSize {
		log.LogWarnf("too small extent size config %v, use default min value %v", size, unit.MinExtentSize)
		c.extentSize = unit.MinExtentSize
		return
	}
	if size&(size-1) != 0 {
		for i := unit.MinExtentSize; ; {
			if i > size {
				c.extentSize = i
				break
			}
			i = i * 2
		}
		log.LogWarnf("invalid extent size %v, need power of 2, use value %v", size, c.extentSize)
		return
	}
	c.extentSize = size
}

func (c *ExtentClient) BackgroundExtentMerge() {
	defer c.wg.Done()
	ctx := context.Background()
	for {
		select {
		case <-c.stopC:
			return
		case <-c.extentMergeChan:
			inodes := c.extentMergeIno
			if len(inodes) == 1 && inodes[0] == 0 {
				inodes = c.lookupAllInode(proto.RootIno)
			}
			for _, inode := range inodes {
				var (
					finish bool
					err    error
				)
				c.OpenStream(inode, false, false)
				for !finish {
					finish, err = c.ExtentMerge(ctx, inode)
					if err != nil {
						log.LogWarnf("BackgroundExtentMerge err: %v, inode(%v)", err, inode)
						break
					}
					time.Sleep(time.Duration(c.ExtentMergeSleepMs) * time.Millisecond)
				}
				c.CloseStream(ctx, inode)
				c.EvictStream(ctx, inode)
			}
		}
	}
}

func (c *ExtentClient) lookupAllInode(parent uint64) (inodes []uint64) {
	ctx := context.Background()
	dentries, err := c.metaWrapper.ReadDir_ll(ctx, parent)
	if err != nil {
		return
	}
	for _, dentry := range dentries {
		if proto.IsRegular(dentry.Type) {
			inodes = append(inodes, dentry.Inode)
		} else if proto.IsDir(dentry.Type) {
			newInodes := c.lookupAllInode(dentry.Inode)
			inodes = append(inodes, newInodes...)
		}
	}
	return
}

func (c *ExtentClient) EnableWriteCache() bool {
	return c.dataWrapper.enableWriteCache
}

func (c *ExtentClient) SetEnableWriteCache(writeCache bool) {
	c.dataWrapper.enableWriteCache = writeCache
}

func (c *ExtentClient) UmpJmtpAddr() string {
	return c.dataWrapper.umpJmtpAddr
}