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
	"net/http"
	"strings"

	"github.com/cubefs/cubefs/apinode/sdk"
	"github.com/cubefs/cubefs/blobstore/common/rpc"
	"github.com/cubefs/cubefs/blobstore/common/trace"
)

// RegisterAPIRouters register drive api handler.
func (d *DriveNode) RegisterAPIRouters() *rpc.Router {
	rpc.RegisterArgsParser(&ArgsListDir{}, "json")
	rpc.RegisterArgsParser(&ArgsPath{}, "json")

	r := rpc.New()

	// set request id and user id at interceptors.
	r.Use(d.setHeaders)

	r.Handle(http.MethodPost, "/v1/drive", d.createDrive)

	r.Handle(http.MethodPost, "/v1/route", d.addUserConfig, rpc.OptArgsQuery())
	r.Handle(http.MethodGet, "/v1/route", d.getUserConfig)

	r.Handle(http.MethodPost, "/v1/meta", nil, rpc.OptArgsQuery())
	r.Handle(http.MethodGet, "/v1/meta", nil, rpc.OptArgsQuery())

	r.Handle(http.MethodGet, "/v1/files", d.handlerListDir, rpc.OptArgsQuery())
	r.Handle(http.MethodPost, "/v1/files", d.mkDir, rpc.OptArgsQuery())

	// file
	r.Handle(http.MethodPut, "/v1/files/upload", d.handleFileUpload, rpc.OptArgsQuery())
	r.Handle(http.MethodPost, "/v1/files/upload", d.handleFileUpload, rpc.OptArgsQuery())
	r.Handle(http.MethodPut, "/v1/files/content", d.handleFileWrite, rpc.OptArgsQuery())
	r.Handle(http.MethodGet, "/v1/files/content", d.handleFileDownload, rpc.OptArgsQuery())
	r.Handle(http.MethodPost, "/v1/files/copy", d.handleFileCopy, rpc.OptArgsQuery())
	r.Handle(http.MethodPost, "/v1/files/rename", d.rename, rpc.OptArgsQuery())
	// file multipart
	r.Handle(http.MethodPost, "/v1/files/multipart", d.handleMultipartUploads, rpc.OptArgsQuery())
	r.Handle(http.MethodPut, "/v1/files/multipart", d.handleMultipartPart, rpc.OptArgsQuery())
	r.Handle(http.MethodGet, "/v1/files/multipart", d.handleMultipartList, rpc.OptArgsQuery())
	r.Handle(http.MethodDelete, "/v1/files/multipart", d.handleMultipartAbort, rpc.OptArgsQuery())

	return r
}

func (*DriveNode) setHeaders(c *rpc.Context) {
	rid := c.Request.Header.Get(headerRequestID)
	c.Set(headerRequestID, rid)

	uid := UserID(c.Request.Header.Get(headerUserID))
	if !uid.Valid() {
		c.AbortWithError(sdk.ErrBadRequest)
		return
	}
	c.Set(headerUserID, uid)
}

func (*DriveNode) requestID(c *rpc.Context) string {
	rid, _ := c.Get(headerRequestID)
	return rid.(string)
}

func (*DriveNode) userID(c *rpc.Context) UserID {
	uid, _ := c.Get(headerUserID)
	return uid.(UserID)
}

func (*DriveNode) getProperties(c *rpc.Context) map[string]string {
	properties := make(map[string]string)
	for key, values := range c.Request.Header {
		if strings.HasPrefix(key, userPropertyPrefix) {
			properties[key[len(userPropertyPrefix):]] = values[0]
		}
	}
	return properties
}

// span carry with request id firstly.
func (d *DriveNode) ctxSpan(c *rpc.Context) (context.Context, trace.Span) {
	ctx := c.Request.Context()
	var span trace.Span
	if rid := d.requestID(c); rid != "" {
		span, _ = trace.StartSpanFromContextWithTraceID(ctx, "", rid)
	} else {
		span = trace.SpanFromContextSafe(ctx)
	}
	return ctx, span
}