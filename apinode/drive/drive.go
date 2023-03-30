// Copyright 2023 The CubeFS Authors.
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

package drive

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/cubefs/cubefs/apinode/sdk/impl"

	"github.com/cubefs/cubefs/apinode/sdk"
	"github.com/cubefs/cubefs/blobstore/util/closer"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/proto"
)

const (
	headerRange = "Range"

	headerRequestID = "x-cfa-request-id"
	headerUserID    = "x-cfa-user-id"
	headerSign      = "x-cfa-sign"

	userPropertyPrefix = "x-cfa-meta-"

	defaultCluster = "defaultCluster"
	defaultVolume  = "defaultVolume"
	XAttrUserKey   = "users"
)

// TODO: defines inode in sdk.

// Inode type of inode.
type Inode uint64

// Uint64 returns uint64.
func (i Inode) Uint64() uint64 {
	return uint64(i)
}

// FileID file id.
type FileID = Inode

// UserID user id.
type UserID string

// Valid return user id is empty or not.
func (u *UserID) Valid() bool {
	return *u != ""
}

type FileInfo struct {
	ID         uint64            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Size       int64             `json:"size"`
	Ctime      int64             `json:"ctime"`
	Mtime      int64             `json:"mtime"`
	Atime      int64             `json:"atime"`
	Properties map[string]string `json:"properties"`
}

func inode2file(ino *sdk.InodeInfo, name string, properties map[string]string) *FileInfo {
	typ := "file"
	if proto.IsDir(ino.Mode) {
		typ = "dir"
	}
	return &FileInfo{
		ID:         ino.Inode,
		Name:       name,
		Type:       typ,
		Size:       int64(ino.Size),
		Ctime:      ino.CreateTime.Unix(),
		Mtime:      ino.ModifyTime.Unix(),
		Atime:      ino.AccessTime.Unix(),
		Properties: properties,
	}
}

type SharedFileInfo struct {
	ID    uint64 `json:"id"`
	Path  string `json:"path"`
	Owner string `json:"owner"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Ctime int64  `json:"ctime"`
	Mtime int64  `json:"mtime"`
	Atime int64  `json:"atime"`
	Perm  string `json:"perm"` // only rd or rw
}

const (
	maxTaskPoolSize = 8
)

type ArgsListDir struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Owner  UserID `json:"owner,omitempty"`
	Marker string `json:"marker,omitempty"`
	Limit  int    `json:"limit"`
	Filter string `json:"filter,omitempty"`
}

type ArgsShare struct {
	Path string `json:"path"`
	Perm string `json:"perm"`
}

type ArgsUnShare struct {
	Path  string `json:"path"`
	Users string `json:"users,omitempty"`
}

type ArgsGetProperties struct {
	Path string `json:"path"`
}

type ArgsSetProperties struct {
	Path string `json:"path"`
}

// DriveNode drive node.
type DriveNode struct {
	defaultVolume sdk.IVolume
	userRouter    *userRouteMgr
	clusterMgr    sdk.ClusterManager

	closer.Closer
}

// New returns a drive node.
func New() *DriveNode {
	ctx := context.Background()
	cm := impl.NewClusterMgr()
	vol := cm.GetCluster(defaultCluster).GetVol(defaultVolume)
	urm, err := NewUserRouteMgr()
	if err != nil {
		log.Fatal(err)
	}
	d := &DriveNode{
		defaultVolume: vol,
		userRouter:    urm,
		clusterMgr:    cm,
		Closer:        closer.New(),
	}
	err = d.initClusterAlloc(ctx)
	if err != nil {
		log.Fatal(err)
	}

	return d
}

// get full path and volume by uid
// filePath is an absolute of client
func (d *DriveNode) getRootInoAndVolume(ctx context.Context, uid UserID) (Inode, sdk.IVolume, error) {
	userRouter, err := d.GetUserRoute(ctx, uid)
	if err != nil {
		return 0, nil, err
	}
	cluster := d.clusterMgr.GetCluster(userRouter.ClusterID)
	if cluster == nil {
		return 0, nil, sdk.ErrNotFound
	}
	volume := cluster.GetVol(userRouter.VolumeID)
	if volume == nil {
		return 0, nil, sdk.ErrNotFound
	}
	return userRouter.RootFileID, volume, nil
}

func (d *DriveNode) lookup(ctx context.Context, vol sdk.IVolume, parentIno Inode, path string) (info *sdk.DirInfo, err error) {
	names := strings.Split(path, "/")
	for _, name := range names {
		if name == "" {
			continue
		}
		info, err = vol.Lookup(ctx, parentIno.Uint64(), name)
		if err != nil {
			return
		}
		parentIno = Inode(info.Inode)
	}
	return
}

func (d *DriveNode) createDir(ctx context.Context, vol sdk.IVolume, parentIno Inode, path string) (info *sdk.InodeInfo, err error) {
	names := strings.Split(path, "/")
	for _, name := range names {
		if name == "" {
			continue
		}
		info, err = vol.Mkdir(ctx, parentIno.Uint64(), name)
		if err != nil && err != sdk.ErrExist {
			return
		} else if err == sdk.ErrExist {
			dirInfo, e := vol.Lookup(ctx, parentIno.Uint64(), name)
			if e != nil {
				return nil, e
			}
			info, err = vol.GetInode(ctx, dirInfo.Inode)
			if err != nil {
				return
			}
			parentIno = Inode(dirInfo.Inode)
		} else {
			parentIno = Inode(info.Inode)
		}
	}
	return
}

func (d *DriveNode) createFile(ctx context.Context, vol sdk.IVolume, parentIno Inode, path string) (info *sdk.InodeInfo, err error) {
	dir, file := filepath.Split(path)
	if file == "" {
		return nil, sdk.ErrBadRequest
	}
	info, err = d.createDir(ctx, vol, parentIno, dir)
	if err != nil {
		return
	}
	info, err = vol.CreateFile(ctx, info.Inode, file)
	if err != nil && err == sdk.ErrExist {
		err = nil
	}
	return
}

func (d *DriveNode) initClusterAlloc(ctx context.Context) (err error) {
	ca := make(map[string]VolumeAlloc)
	clusterVols := d.getClusterAndVolumes()
	for c, vols := range clusterVols {
		va := make(VolumeAlloc)
		for _, v := range vols {
			va[v] = 0
		}
		ca[c] = va
	}
	b, err := json.Marshal(ca)
	if err != nil {
		return
	}
	fileInfo, err := d.lookup(ctx, d.defaultVolume, 0, volAllocPath)
	if err != nil {
		return
	}
	err = d.defaultVolume.SetXAttr(ctx, fileInfo.Inode, "clusters", string(b))
	if err != nil {
		return
	}
	return
}