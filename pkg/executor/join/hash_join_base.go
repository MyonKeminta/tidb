// Copyright 2024 PingCAP, Inc.
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

package join

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/planner/core/operator/logicalop"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/disk"
	"github.com/pingcap/tidb/pkg/util/memory"
)

// hashjoinWorkerResult stores the result of join workers,
// `src` is for Chunk reuse: the main goroutine will get the join result chunk `chk`,
// and push `chk` into `src` after processing, join worker goroutines get the empty chunk from `src`
// and push new data into this chunk.
type hashjoinWorkerResult struct {
	chk *chunk.Chunk
	err error
	src chan<- *chunk.Chunk
}

type hashJoinCtxBase struct {
	SessCtx        sessionctx.Context
	ChunkAllocPool chunk.Allocator
	// Concurrency is the number of partition, build and join workers.
	Concurrency  uint
	joinResultCh chan *hashjoinWorkerResult
	// closeCh add a lock for closing executor.
	closeCh       chan struct{}
	finished      atomic.Bool
	IsNullEQ      []bool
	buildFinished chan error
	JoinType      logicalop.JoinType
	IsNullAware   bool
	memTracker    *memory.Tracker // track memory usage.
	diskTracker   *disk.Tracker   // track disk usage.
}

type probeSideTupleFetcherBase struct {
	ProbeSideExec       exec.Executor
	probeChkResourceCh  chan *probeChkResource
	probeResultChs      []chan *chunk.Chunk
	requiredRows        int64
	joinResultChannel   chan *hashjoinWorkerResult
	canSkipScanRowTable bool
}

func (fetcher *probeSideTupleFetcherBase) initializeForProbeBase(concurrency uint, joinResultChannel chan *hashjoinWorkerResult) {
	// fetcher.probeResultChs is for transmitting the chunks which store the data of
	// ProbeSideExec, it'll be written by probe side worker goroutine, and read by join
	// workers.
	fetcher.probeResultChs = make([]chan *chunk.Chunk, concurrency)
	for i := uint(0); i < concurrency; i++ {
		fetcher.probeResultChs[i] = make(chan *chunk.Chunk, 1)
	}
	// fetcher.probeChkResourceCh is for transmitting the used ProbeSideExec chunks from
	// join workers to ProbeSideExec worker.
	fetcher.probeChkResourceCh = make(chan *probeChkResource, concurrency)
	for i := uint(0); i < concurrency; i++ {
		fetcher.probeChkResourceCh <- &probeChkResource{
			chk:  exec.NewFirstChunk(fetcher.ProbeSideExec),
			dest: fetcher.probeResultChs[i],
		}
	}
	fetcher.joinResultChannel = joinResultChannel
}

func (fetcher *probeSideTupleFetcherBase) handleProbeSideFetcherPanic(r any) {
	for i := range fetcher.probeResultChs {
		close(fetcher.probeResultChs[i])
	}
	if r != nil {
		fetcher.joinResultChannel <- &hashjoinWorkerResult{err: util.GetRecoverError(r)}
	}
}

type isBuildSideEmpty func() bool
type isSpillTriggered func() bool

func wait4BuildSide(isBuildEmpty isBuildSideEmpty, checkSpill isSpillTriggered, canSkipIfBuildEmpty, needScanAfterProbeDone bool, hashJoinCtx *hashJoinCtxBase) (skipProbe bool, skipScanRowTable bool) {
	var err error
	skipProbe = false
	skipScanRowTable = false
	buildFinishes := false
	select {
	case <-hashJoinCtx.closeCh:
		// current executor is closed, no need to probe
		skipProbe = true
		skipScanRowTable = true
	case err = <-hashJoinCtx.buildFinished:
		if err != nil {
			// build meet error, no need to probe
			skipProbe = true
			skipScanRowTable = true
		} else {
			buildFinishes = true
		}
	}
	// only check build empty if build finishes
	if buildFinishes && isBuildEmpty() && !checkSpill() && canSkipIfBuildEmpty {
		// if build side is empty, can skip probe if canSkipIfBuildEmpty is true(e.g. inner join)
		skipProbe = true
	}
	if err != nil {
		// if err is not nil, send out the error
		hashJoinCtx.joinResultCh <- &hashjoinWorkerResult{
			err: err,
		}
	} else if skipProbe {
		// if skipProbe is true and there is no need to scan hash table after probe, just the whole hash join is finished
		if !needScanAfterProbeDone {
			hashJoinCtx.finished.Store(true)
		}
	}
	return skipProbe, skipScanRowTable
}

func (fetcher *probeSideTupleFetcherBase) getProbeSideResource(shouldLimitProbeFetchSize bool, maxChunkSize int, hashJoinCtx *hashJoinCtxBase) *probeChkResource {
	if hashJoinCtx.finished.Load() {
		return nil
	}

	var probeSideResource *probeChkResource
	var ok bool
	select {
	case <-hashJoinCtx.closeCh:
		return nil
	case probeSideResource, ok = <-fetcher.probeChkResourceCh:
		if !ok {
			return nil
		}
	}
	if shouldLimitProbeFetchSize {
		required := int(atomic.LoadInt64(&fetcher.requiredRows))
		probeSideResource.chk.SetRequiredRows(required, maxChunkSize)
	}
	return probeSideResource
}

// fetchProbeSideChunks get chunks from fetches chunks from the big table in a background goroutine
// and sends the chunks to multiple channels which will be read by multiple join workers.
func (fetcher *probeSideTupleFetcherBase) fetchProbeSideChunks(ctx context.Context, maxChunkSize int, isBuildEmpty isBuildSideEmpty, checkSpill isSpillTriggered, canSkipIfBuildEmpty, needScanAfterProbeDone, shouldLimitProbeFetchSize bool, hashJoinCtx *hashJoinCtxBase) {
	hasWaitedForBuild := false
	for {
		probeSideResource := fetcher.getProbeSideResource(shouldLimitProbeFetchSize, maxChunkSize, hashJoinCtx)
		if probeSideResource == nil {
			return
		}
		probeSideResult := probeSideResource.chk
		err := exec.Next(ctx, fetcher.ProbeSideExec, probeSideResult)
		failpoint.Inject("ConsumeRandomPanic", nil)
		if err != nil {
			hashJoinCtx.joinResultCh <- &hashjoinWorkerResult{
				err: err,
			}
			return
		}

		err = triggerIntest(2)
		if err != nil {
			hashJoinCtx.joinResultCh <- &hashjoinWorkerResult{
				err: err,
			}
			return
		}

		if !hasWaitedForBuild {
			failpoint.Inject("issue30289", func(val failpoint.Value) {
				if val.(bool) {
					probeSideResult.Reset()
				}
			})
			skipProbe, skipScanRowTable := wait4BuildSide(isBuildEmpty, checkSpill, canSkipIfBuildEmpty, needScanAfterProbeDone, hashJoinCtx)
			if skipScanRowTable {
				fetcher.canSkipScanRowTable = true
			}
			if skipProbe {
				// there is no need to probe, so just return
				return
			}
			hasWaitedForBuild = true
		}

		if probeSideResult.NumRows() == 0 {
			return
		}

		probeSideResource.dest <- probeSideResult
	}
}

type probeWorkerBase struct {
	WorkerID           uint
	probeChkResourceCh chan *probeChkResource
	joinChkResourceCh  chan *chunk.Chunk
	probeResultCh      chan *chunk.Chunk
}

func (worker *probeWorkerBase) initializeForProbe(probeChkResourceCh chan *probeChkResource, probeResultCh chan *chunk.Chunk, joinExec exec.Executor) {
	// worker.joinChkResourceCh is for transmitting the reused join result chunks
	// from the main thread to probe worker goroutines.
	worker.joinChkResourceCh = make(chan *chunk.Chunk, 1)
	worker.joinChkResourceCh <- exec.NewFirstChunk(joinExec)
	worker.probeChkResourceCh = probeChkResourceCh
	worker.probeResultCh = probeResultCh
}

type buildWorkerBase struct {
	BuildSideExec  exec.Executor
	BuildKeyColIdx []int
}

func syncerAdd(syncer *sync.WaitGroup) {
	if syncer != nil {
		syncer.Add(1)
	}
}

func syncerDone(syncer *sync.WaitGroup) {
	if syncer != nil {
		syncer.Done()
	}
}

func checkAndSpillRowTableIfNeeded(fetcherAndWorkerSyncer *sync.WaitGroup, spillHelper *hashJoinSpillHelper) error {
	if fetcherAndWorkerSyncer == nil {
		return nil
	}

	if spillHelper.isSpillNeeded() {
		// Wait for the stop of all workers
		fetcherAndWorkerSyncer.Wait()
		return spillHelper.spillRowTable(nil)
	}
	return nil
}

// fetchBuildSideRows fetches all rows from build side executor, and append them
// to e.buildSideResult.
func (w *buildWorkerBase) fetchBuildSideRows(ctx context.Context, hashJoinCtx *hashJoinCtxBase, fetcherAndWorkerSyncer *sync.WaitGroup, spillHelper *hashJoinSpillHelper, chkCh chan<- *chunk.Chunk, errCh chan<- error, doneCh <-chan struct{}) {
	hasError := false

	// We must put the close of chkCh after the place of spilling remaining rows or there will be data race
	defer close(chkCh)

	defer func() {
		if r := recover(); r != nil {
			errCh <- util.GetRecoverError(r)
			return
		}

		if hasError {
			return
		}

		if fetcherAndWorkerSyncer != nil {
			if spillHelper.isSpillTriggered() {
				// Spill remaining rows
				fetcherAndWorkerSyncer.Wait()
				err := spillHelper.spillRemainingRows()
				if err != nil {
					errCh <- errors.Trace(err)
				}
			}
		}
	}()

	var err error
	failpoint.Inject("issue30289", func(val failpoint.Value) {
		if val.(bool) {
			err = errors.Errorf("issue30289 build return error")
			errCh <- errors.Trace(err)
			return
		}
	})

	failpoint.Inject("issue42662_1", func(val failpoint.Value) {
		if val.(bool) {
			if hashJoinCtx.SessCtx.GetSessionVars().ConnectionID != 0 {
				// consume 170MB memory, this sql should be tracked into MemoryTop1Tracker
				hashJoinCtx.memTracker.Consume(170 * 1024 * 1024)
			}
			return
		}
	})

	sessVars := hashJoinCtx.SessCtx.GetSessionVars()
	failpoint.Inject("issue51998", func(val failpoint.Value) {
		if val.(bool) {
			time.Sleep(2 * time.Second)
		}
	})

	for {
		err := checkAndSpillRowTableIfNeeded(fetcherAndWorkerSyncer, spillHelper)
		issue59377Intest(&err)
		if err != nil {
			hasError = true
			errCh <- errors.Trace(err)
			return
		}

		err = triggerIntest(2)
		if err != nil {
			hasError = true
			errCh <- errors.Trace(err)
			return
		}

		if hashJoinCtx.finished.Load() {
			return
		}

		chk := hashJoinCtx.ChunkAllocPool.Alloc(w.BuildSideExec.RetFieldTypes(), sessVars.MaxChunkSize, sessVars.MaxChunkSize)
		err = exec.Next(ctx, w.BuildSideExec, chk)

		failpoint.Inject("issue51998", func(val failpoint.Value) {
			if val.(bool) {
				hasError = true
				err = errors.Errorf("issue51998 build return error")
			}
		})

		if err != nil {
			hasError = true
			errCh <- errors.Trace(err)
			return
		}

		failpoint.Inject("errorFetchBuildSideRowsMockOOMPanic", nil)
		failpoint.Inject("ConsumeRandomPanic", nil)

		if chk.NumRows() == 0 {
			return
		}

		syncerAdd(fetcherAndWorkerSyncer)
		select {
		case <-doneCh:
			syncerDone(fetcherAndWorkerSyncer)
			return
		case <-hashJoinCtx.closeCh:
			syncerDone(fetcherAndWorkerSyncer)
			return
		case chkCh <- chk:
		}
	}
}

// probeChkResource stores the result of the join probe side fetch worker,
// `dest` is for Chunk reuse: after join workers process the probe side chunk which is read from `dest`,
// they'll store the used chunk as `chk`, and then the probe side fetch worker will put new data into `chk` and write `chk` into dest.
type probeChkResource struct {
	chk  *chunk.Chunk
	dest chan<- *chunk.Chunk
}
