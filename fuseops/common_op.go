// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fuseops

import (
	"flag"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jacobsa/bazilfuse"
	"github.com/jacobsa/reqtrace"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

var fTraceByPID = flag.Bool(
	"fuse.trace_by_pid",
	false,
	"Enable a hacky mode that uses reqtrace to group all ops from each "+
		"individual PID. Not a good idea to use in production; races, bugs, and "+
		"resource leaks likely lurk.")

// A helper for embedding common behavior.
type commonOp struct {
	// The context exposed to the user.
	ctx context.Context

	// The op in which this struct is embedded.
	op Op

	// The underlying bazilfuse request for this op.
	bazilReq bazilfuse.Request

	// A function that can be used to log information about the op. The first
	// argument is a call depth.
	log func(int, string, ...interface{})

	// A function that is invoked with the error given to Respond, for use in
	// closing off traces and reporting back to the connection.
	finished func(error)
}

var gPIDMapMu sync.Mutex

// A map from PID to a traced context for that PID.
//
// GUARDED_BY(gPIDMapMu)
var gPIDMap = make(map[int]context.Context)

// Wait until the process completes, then close off the trace and remove the
// context from the map.
func reportWhenPIDGone(
	pid int,
	ctx context.Context,
	report reqtrace.ReportFunc) {
	// HACK(jacobsa): Poll until the process no longer exists.
	const pollPeriod = 50 * time.Millisecond
	for {
		// The man page for kill(2) says that if the signal is zero, then "no
		// signal is sent, but error checking is still performed; this can be used
		// to check for the existence of a process ID".
		err := unix.Kill(pid, 0)

		// ESRCH means the process is gone.
		if err == unix.ESRCH {
			break
		}

		// If we receive EPERM, we're not going to be able to do what we want. We
		// don't really have any choice but to print info and leak.
		if err == unix.EPERM {
			log.Printf("Failed to kill(2) PID %v; no permissions. Leaking trace.", pid)
			return
		}

		// Otherwise, panic.
		if err != nil {
			panic(fmt.Errorf("Kill(%v): %v", pid, err))
		}

		time.Sleep(pollPeriod)
	}

	// Finish up.
	report(nil)

	gPIDMapMu.Lock()
	delete(gPIDMap, pid)
	gPIDMapMu.Unlock()
}

func (o *commonOp) maybeTraceByPID(
	in context.Context,
	pid int) (out context.Context) {
	// Is there anything to do?
	if !reqtrace.Enabled() || !*fTraceByPID {
		out = in
		return
	}

	gPIDMapMu.Lock()
	defer gPIDMapMu.Unlock()

	// Do we already have a traced context for this PID?
	if existing, ok := gPIDMap[pid]; ok {
		out = existing
		return
	}

	// Set up a new one and stick it in the map.
	var report reqtrace.ReportFunc
	out, report = reqtrace.Trace(in, fmt.Sprintf("Requests from PID %v", pid))
	gPIDMap[pid] = out

	// Ensure we close the trace and remove it from the map eventually.
	go reportWhenPIDGone(pid, out, report)

	return
}

func (o *commonOp) ShortDesc() (desc string) {
	opName := reflect.TypeOf(o.op).String()

	// Attempt to better handle the usual case: a string that looks like
	// "*fuseops.GetInodeAttributesOp".
	const prefix = "*fuseops."
	const suffix = "Op"
	if strings.HasPrefix(opName, prefix) && strings.HasSuffix(opName, suffix) {
		opName = opName[len(prefix) : len(opName)-len(suffix)]
	}

	// Include the inode number to which the op applies.
	desc = fmt.Sprintf("%s(inode=%v)", opName, o.bazilReq.Hdr().Node)

	return
}

func (o *commonOp) init(
	ctx context.Context,
	op Op,
	bazilReq bazilfuse.Request,
	log func(int, string, ...interface{}),
	finished func(error)) {
	// Initialize basic fields.
	o.ctx = ctx
	o.op = op
	o.bazilReq = bazilReq
	o.log = log
	o.finished = finished

	// Set up a context that reflects per-PID tracing if appropriate.
	o.ctx = o.maybeTraceByPID(o.ctx, int(bazilReq.Hdr().Pid))

	// Set up a trace span for this op.
	var reportForTrace reqtrace.ReportFunc
	o.ctx, reportForTrace = reqtrace.StartSpan(ctx, o.op.ShortDesc())

	// When the op is finished, report to both reqtrace and the connection.
	prevFinish := o.finished
	o.finished = func(err error) {
		reportForTrace(err)
		prevFinish(err)
	}
}

func (o *commonOp) Header() OpHeader {
	bh := o.bazilReq.Hdr()
	return OpHeader{
		Uid: bh.Uid,
		Gid: bh.Gid,
	}
}

func (o *commonOp) Context() context.Context {
	return o.ctx
}

func (o *commonOp) Logf(format string, v ...interface{}) {
	const calldepth = 2
	o.log(calldepth, format, v...)
}

func (o *commonOp) respondErr(err error) {
	if err == nil {
		panic("Expect non-nil here.")
	}

	// Don't forget to report back to the connection that we are finished.
	defer o.finished(err)

	// Log that we are finished.
	o.Logf(
		"-> (%s) error: %v",
		o.op.ShortDesc(),
		err)

	// Send a response to the kernel.
	o.bazilReq.RespondError(err)
}

// Respond with the supplied response struct, which must be accepted by a
// method called Respond on o.bazilReq.
//
// Special case: nil means o.bazilReq.Respond accepts no parameters.
func (o *commonOp) respond(resp interface{}) {
	// Don't forget to report back to the connection that we are finished.
	defer o.finished(nil)

	// Find the Respond method.
	v := reflect.ValueOf(o.bazilReq)
	respond := v.MethodByName("Respond")

	// Special case: handle successful ops with no response struct.
	if resp == nil {
		o.Logf("-> (%s) OK", o.op.ShortDesc())
		respond.Call([]reflect.Value{})
		return
	}

	// Otherwise, send the response struct to the kernel.
	o.Logf("-> %v", resp)
	respond.Call([]reflect.Value{reflect.ValueOf(resp)})
}
