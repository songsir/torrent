package torrent

import (
	"container/heap"
	"context"
	"encoding/gob"
	"math/rand"
	"reflect"
	"runtime/pprof"
	"time"
	"unsafe"

	"github.com/anacrolix/log"
	"github.com/anacrolix/multiless"
	"github.com/anacrolix/torrent/metainfo"

	request_strategy "github.com/anacrolix/torrent/request-strategy"
)

// Returns what is necessary to run request_strategy.GetRequestablePieces for primaryTorrent.
func (cl *Client) getRequestStrategyInput(primaryTorrent *Torrent) (input request_strategy.Input) {
	input.MaxUnverifiedBytes = cl.config.MaxUnverifiedBytes
	if !primaryTorrent.haveInfo() {
		return
	}
	if capFunc := primaryTorrent.storage.Capacity; capFunc != nil {
		if cap, ok := (*capFunc)(); ok {
			input.Capacity = &cap
		}
	}
	input.Torrents = make(map[metainfo.Hash]request_strategy.Torrent, len(cl.torrents))
	for _, t := range cl.torrents {
		if !t.haveInfo() {
			// This would be removed if metadata is handled here. Determining chunks per piece
			// requires the info. If we have no info, we have no pieces too, so the end result is
			// the same.
			continue
		}
		if t.storage.Capacity != primaryTorrent.storage.Capacity {
			continue
		}
		input.Torrents[t.infoHash] = t.requestStrategyTorrentInput()
	}
	return
}

func (t *Torrent) getRequestStrategyInput() request_strategy.Input {
	return t.cl.getRequestStrategyInput(t)
}

func (t *Torrent) requestStrategyTorrentInput() request_strategy.Torrent {
	rst := request_strategy.Torrent{
		InfoHash:       t.infoHash,
		ChunksPerPiece: t.chunksPerRegularPiece(),
	}
	rst.Pieces = make([]request_strategy.Piece, 0, len(t.pieces))
	for i := range t.pieces {
		rst.Pieces = append(rst.Pieces, t.makeRequestStrategyPiece(i))
	}
	return rst
}

func (t *Torrent) requestStrategyPieceOrderState(i int) request_strategy.PieceRequestOrderState {
	return request_strategy.PieceRequestOrderState{
		Priority:     t.piece(i).purePriority(),
		Partial:      t.piecePartiallyDownloaded(i),
		Availability: t.piece(i).availability,
	}
}

func (t *Torrent) makeRequestStrategyPiece(i int) request_strategy.Piece {
	p := &t.pieces[i]
	return request_strategy.Piece{
		Request:           !t.ignorePieceForRequests(i),
		Priority:          p.purePriority(),
		Partial:           t.piecePartiallyDownloaded(i),
		Availability:      p.availability,
		Length:            int64(p.length()),
		NumPendingChunks:  int(t.pieceNumPendingChunks(i)),
		IterPendingChunks: &p.undirtiedChunksIter,
	}
}

func init() {
	gob.Register(peerId{})
}

type peerId struct {
	*Peer
	ptr uintptr
}

func (p peerId) Uintptr() uintptr {
	return p.ptr
}

func (p peerId) GobEncode() (b []byte, _ error) {
	*(*reflect.SliceHeader)(unsafe.Pointer(&b)) = reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&p.ptr)),
		Len:  int(unsafe.Sizeof(p.ptr)),
		Cap:  int(unsafe.Sizeof(p.ptr)),
	}
	return
}

func (p *peerId) GobDecode(b []byte) error {
	if uintptr(len(b)) != unsafe.Sizeof(p.ptr) {
		panic(len(b))
	}
	ptr := unsafe.Pointer(&b[0])
	p.ptr = *(*uintptr)(ptr)
	log.Printf("%p", ptr)
	dst := reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&p.Peer)),
		Len:  int(unsafe.Sizeof(p.Peer)),
		Cap:  int(unsafe.Sizeof(p.Peer)),
	}
	copy(*(*[]byte)(unsafe.Pointer(&dst)), b)
	return nil
}

type (
	RequestIndex   = request_strategy.RequestIndex
	chunkIndexType = request_strategy.ChunkIndex
)

type peerRequests struct {
	requestIndexes       []RequestIndex
	peer                 *Peer
	torrentStrategyInput request_strategy.Torrent
}

func (p *peerRequests) Len() int {
	return len(p.requestIndexes)
}

func (p *peerRequests) Less(i, j int) bool {
	leftRequest := p.requestIndexes[i]
	rightRequest := p.requestIndexes[j]
	t := p.peer.t
	leftPieceIndex := leftRequest / p.torrentStrategyInput.ChunksPerPiece
	rightPieceIndex := rightRequest / p.torrentStrategyInput.ChunksPerPiece
	leftCurrent := p.peer.actualRequestState.Requests.Contains(leftRequest)
	rightCurrent := p.peer.actualRequestState.Requests.Contains(rightRequest)
	pending := func(index RequestIndex, current bool) int {
		ret := t.pendingRequests.Get(index)
		if current {
			ret--
		}
		// See https://github.com/anacrolix/torrent/issues/679 for possible issues. This should be
		// resolved.
		if ret < 0 {
			panic(ret)
		}
		return ret
	}
	ml := multiless.New()
	// Push requests that can't be served right now to the end. But we don't throw them away unless
	// there's a better alternative. This is for when we're using the fast extension and get choked
	// but our requests could still be good when we get unchoked.
	if p.peer.peerChoking {
		ml = ml.Bool(
			!p.peer.peerAllowedFast.Contains(leftPieceIndex),
			!p.peer.peerAllowedFast.Contains(rightPieceIndex),
		)
	}
	ml = ml.Int(
		pending(leftRequest, leftCurrent),
		pending(rightRequest, rightCurrent))
	ml = ml.Bool(!leftCurrent, !rightCurrent)
	ml = ml.Int(
		-int(p.torrentStrategyInput.Pieces[leftPieceIndex].Priority),
		-int(p.torrentStrategyInput.Pieces[rightPieceIndex].Priority),
	)
	ml = ml.Int(
		int(p.torrentStrategyInput.Pieces[leftPieceIndex].Availability),
		int(p.torrentStrategyInput.Pieces[rightPieceIndex].Availability))
	ml = ml.Uint32(leftPieceIndex, rightPieceIndex)
	ml = ml.Uint32(leftRequest, rightRequest)
	return ml.MustLess()
}

func (p *peerRequests) Swap(i, j int) {
	p.requestIndexes[i], p.requestIndexes[j] = p.requestIndexes[j], p.requestIndexes[i]
}

func (p *peerRequests) Push(x interface{}) {
	p.requestIndexes = append(p.requestIndexes, x.(RequestIndex))
}

func (p *peerRequests) Pop() interface{} {
	last := len(p.requestIndexes) - 1
	x := p.requestIndexes[last]
	p.requestIndexes = p.requestIndexes[:last]
	return x
}

type desiredRequestState struct {
	Requests   []RequestIndex
	Interested bool
}

func (p *Peer) getDesiredRequestState() (desired desiredRequestState) {
	input := p.t.getRequestStrategyInput()
	requestHeap := peerRequests{
		peer: p,
	}
	requestHeap.torrentStrategyInput = input.Torrents[p.t.infoHash]
	request_strategy.GetRequestablePieces(
		input,
		p.t.cl.pieceRequestOrder[p.t.storage.Capacity],
		func(t *request_strategy.Torrent, rsp *request_strategy.Piece, pieceIndex int) {
			if t.InfoHash != p.t.infoHash {
				return
			}
			if !p.peerHasPiece(pieceIndex) {
				return
			}
			allowedFast := p.peerAllowedFast.ContainsInt(pieceIndex)
			rsp.IterPendingChunks.Iter(func(ci request_strategy.ChunkIndex) {
				r := p.t.pieceRequestIndexOffset(pieceIndex) + ci
				//if p.t.pendingRequests.Get(r) != 0 && !p.actualRequestState.Requests.Contains(r) {
				//	return
				//}
				if !allowedFast {
					// We must signal interest to request this
					desired.Interested = true
					// We can make or will allow sustaining a request here if we're not choked, or
					// have made the request previously (presumably while unchoked), and haven't had
					// the peer respond yet (and the request was retained because we are using the
					// fast extension).
					if p.peerChoking && !p.actualRequestState.Requests.Contains(r) {
						// We can't request this right now.
						return
					}
				}
				requestHeap.requestIndexes = append(requestHeap.requestIndexes, r)
			})
		},
	)
	p.t.assertPendingRequests()
	heap.Init(&requestHeap)
	for requestHeap.Len() != 0 && len(desired.Requests) < p.nominalMaxRequests() {
		requestIndex := heap.Pop(&requestHeap).(RequestIndex)
		desired.Requests = append(desired.Requests, requestIndex)
	}
	return
}

func (p *Peer) maybeUpdateActualRequestState() bool {
	if p.needRequestUpdate == "" {
		return true
	}
	var more bool
	pprof.Do(
		context.Background(),
		pprof.Labels("update request", p.needRequestUpdate),
		func(_ context.Context) {
			next := p.getDesiredRequestState()
			more = p.applyRequestState(next)
		},
	)
	return more
}

// Transmit/action the request state to the peer.
func (p *Peer) applyRequestState(next desiredRequestState) bool {
	current := &p.actualRequestState
	if !p.setInterested(next.Interested) {
		return false
	}
	more := true
	cancel := current.Requests.Clone()
	for _, ri := range next.Requests {
		cancel.Remove(ri)
	}
	cancel.Iterate(func(req uint32) bool {
		more = p.cancel(req)
		return more
	})
	if !more {
		return false
	}
	shuffled := false
	lastPending := 0
	for i := 0; i < len(next.Requests); i++ {
		req := next.Requests[i]
		if p.cancelledRequests.Contains(req) {
			// Waiting for a reject or piece message, which will suitably trigger us to update our
			// requests, so we can skip this one with no additional consideration.
			continue
		}
		// The cardinality of our desired requests shouldn't exceed the max requests since it's used
		// in the calculation of the requests. However, if we cancelled requests and they haven't
		// been rejected or serviced yet with the fast extension enabled, we can end up with more
		// extra outstanding requests. We could subtract the number of outstanding cancels from the
		// next request cardinality, but peers might not like that.
		if maxRequests(current.Requests.GetCardinality()) >= p.nominalMaxRequests() {
			//log.Printf("not assigning all requests [desired=%v, cancelled=%v, current=%v, max=%v]",
			//	next.Requests.GetCardinality(),
			//	p.cancelledRequests.GetCardinality(),
			//	current.Requests.GetCardinality(),
			//	p.nominalMaxRequests(),
			//)
			break
		}
		otherPending := p.t.pendingRequests.Get(next.Requests[0])
		if p.actualRequestState.Requests.Contains(next.Requests[0]) {
			otherPending--
		}
		if otherPending < lastPending {
			// Pending should only rise. It's supposed to be the strongest ordering criteria. If it
			// doesn't, our shuffling condition could be wrong.
			panic(lastPending)
		}
		// If the request has already been requested by another peer, shuffle this and the rest of
		// the requests (since according to the increasing condition, the rest of the indices
		// already have an outstanding request with another peer).
		if !shuffled && otherPending > 0 {
			shuffleReqs := next.Requests[i:]
			rand.Shuffle(len(shuffleReqs), func(i, j int) {
				shuffleReqs[i], shuffleReqs[j] = shuffleReqs[j], shuffleReqs[i]
			})
			log.Printf("shuffled reqs [%v:%v]", i, len(next.Requests))
			shuffled = true
			// Repeat this index
			i--
			continue
		}

		more = p.mustRequest(req)
		if !more {
			break
		}
	}
	p.updateRequestsTimer.Stop()
	if more {
		p.needRequestUpdate = ""
		if !current.Requests.IsEmpty() {
			p.updateRequestsTimer.Reset(3 * time.Second)
		}
	}
	return more
}
