package request_strategy

import (
	"bytes"
	"expvar"
	"runtime"
	"sort"
	"sync"

	"github.com/anacrolix/multiless"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/anacrolix/torrent/types"
)

type (
	RequestIndex  = uint32
	ChunkIndex    = uint32
	Request       = types.Request
	pieceIndex    = types.PieceIndex
	piecePriority = types.PiecePriority
	// This can be made into a type-param later, will be great for testing.
	ChunkSpec = types.ChunkSpec
)

type ClientPieceOrder struct{}

func equalFilterPieces(l, r []filterPiece) bool {
	if len(l) != len(r) {
		return false
	}
	for i := range l {
		lp := &l[i]
		rp := &r[i]
		if lp.Priority != rp.Priority ||
			lp.Partial != rp.Partial ||
			lp.Availability != rp.Availability ||
			lp.index != rp.index ||
			lp.t.InfoHash != rp.t.InfoHash {
			return false
		}
	}
	return true
}

func sortFilterPieces(pieces []filterPiece, indices []int) {
	sort.Slice(indices, func(_i, _j int) bool {
		i := &pieces[indices[_i]]
		j := &pieces[indices[_j]]
		return multiless.New().Int(
			int(j.Priority), int(i.Priority),
		).Bool(
			j.Partial, i.Partial,
		).Int64(
			i.Availability, j.Availability,
		).Int(
			i.index, j.index,
		).Lazy(func() multiless.Computation {
			return multiless.New().Cmp(bytes.Compare(
				i.t.InfoHash[:],
				j.t.InfoHash[:],
			))
		}).MustLess()
	})
}

type requestsPeer struct {
	Peer
	nextState                  PeerNextRequestState
	requestablePiecesRemaining int
}

func (rp *requestsPeer) canFitRequest() bool {
	return int(rp.nextState.Requests.GetCardinality()) < rp.MaxRequests
}

func (rp *requestsPeer) addNextRequest(r RequestIndex) {
	if !rp.nextState.Requests.CheckedAdd(r) {
		panic("should only add once")
	}
}

type peersForPieceRequests struct {
	requestsInPiece int
	*requestsPeer
}

func (me *peersForPieceRequests) addNextRequest(r RequestIndex) {
	me.requestsPeer.addNextRequest(r)
	me.requestsInPiece++
}

type requestablePiece struct {
	index             pieceIndex
	t                 *Torrent
	alwaysReallocate  bool
	NumPendingChunks  int
	IterPendingChunks ChunksIterFunc
}

func (p *requestablePiece) chunkIndexToRequestIndex(c ChunkIndex) RequestIndex {
	return p.t.ChunksPerPiece*uint32(p.index) + c
}

type filterPiece struct {
	t     *Torrent
	index pieceIndex
	*Piece
}

var (
	sortsMu sync.Mutex
	sorts   = map[*[]filterPiece][]int{}
)

func reorderedFilterPieces(pieces []filterPiece, indices []int) (ret []filterPiece) {
	ret = make([]filterPiece, len(indices))
	for i, j := range indices {
		ret[i] = pieces[j]
	}
	return
}

var packageExpvarMap = expvar.NewMap("request-strategy")

func getSortedFilterPieces(unsorted []filterPiece) []filterPiece {
	sortsMu.Lock()
	defer sortsMu.Unlock()
	for key, order := range sorts {
		if equalFilterPieces(*key, unsorted) {
			packageExpvarMap.Add("reused filter piece ordering", 1)
			return reorderedFilterPieces(unsorted, order)
		}
	}
	sorted := append(make([]filterPiece, 0, len(unsorted)), unsorted...)
	indices := make([]int, len(sorted))
	for i := 0; i < len(indices); i++ {
		indices[i] = i
	}
	sortFilterPieces(sorted, indices)
	packageExpvarMap.Add("added filter piece ordering", 1)
	sorts[&unsorted] = indices
	runtime.SetFinalizer(&pieceOrderingFinalizer{unsorted: &unsorted}, func(me *pieceOrderingFinalizer) {
		packageExpvarMap.Add("finalized filter piece ordering", 1)
		sortsMu.Lock()
		defer sortsMu.Unlock()
		delete(sorts, me.unsorted)
	})
	return reorderedFilterPieces(unsorted, indices)
}

type pieceOrderingFinalizer struct {
	unsorted *[]filterPiece
}

// Calls f with requestable pieces in order.
func GetRequestablePieces(input Input, f func(t *Torrent, p *Piece, pieceIndex int)) {
	maxPieces := 0
	for i := range input.Torrents {
		maxPieces += len(input.Torrents[i].Pieces)
	}
	pieces := make([]filterPiece, 0, maxPieces)
	// Storage capacity left for this run, keyed by the storage capacity pointer on the storage
	// TorrentImpl. A nil value means no capacity limit.
	var storageLeft *int64
	if input.Capacity != nil {
		storageLeft = new(int64)
		*storageLeft = *input.Capacity
	}
	for _t := range input.Torrents {
		// TODO: We could do metainfo requests here.
		t := &input.Torrents[_t]
		for i := range t.Pieces {
			pieces = append(pieces, filterPiece{
				t:     &input.Torrents[_t],
				index: i,
				Piece: &t.Pieces[i],
			})
		}
	}
	pieces = getSortedFilterPieces(pieces)
	var allTorrentsUnverifiedBytes int64
	torrentUnverifiedBytes := map[metainfo.Hash]int64{}
	for _, piece := range pieces {
		if left := storageLeft; left != nil {
			if *left < piece.Length {
				continue
			}
			*left -= piece.Length
		}
		if !piece.Request || piece.NumPendingChunks == 0 {
			// TODO: Clarify exactly what is verified. Stuff that's being hashed should be
			// considered unverified and hold up further requests.
			continue
		}
		if piece.t.MaxUnverifiedBytes != 0 && torrentUnverifiedBytes[piece.t.InfoHash]+piece.Length > piece.t.MaxUnverifiedBytes {
			continue
		}
		if input.MaxUnverifiedBytes != 0 && allTorrentsUnverifiedBytes+piece.Length > input.MaxUnverifiedBytes {
			continue
		}
		torrentUnverifiedBytes[piece.t.InfoHash] += piece.Length
		allTorrentsUnverifiedBytes += piece.Length
		f(piece.t, piece.Piece, piece.index)
	}
	return
}

type Input struct {
	// This is all torrents that share the same capacity below (or likely a single torrent if there
	// is infinite capacity, since you could just run it separately for each Torrent if that's the
	// case).
	Torrents []Torrent
	// Must not be modified. Non-nil if capacity is not infinite, meaning that pieces of torrents
	// that share the same capacity key must be incorporated in piece ordering.
	Capacity *int64
	// Across all the Torrents. This might be partitioned by storage capacity key now.
	MaxUnverifiedBytes int64
}

// Checks that a sorted peersForPiece slice makes sense.
func ensureValidSortedPeersForPieceRequests(peers *peersForPieceSorter) {
	if !sort.IsSorted(peers) {
		panic("not sorted")
	}
	peerMap := make(map[*peersForPieceRequests]struct{}, peers.Len())
	for _, p := range peers.peersForPiece {
		if _, ok := peerMap[p]; ok {
			panic(p)
		}
		peerMap[p] = struct{}{}
	}
}

var peersForPiecesPool sync.Pool

func makePeersForPiece(cap int) []*peersForPieceRequests {
	got := peersForPiecesPool.Get()
	if got == nil {
		return make([]*peersForPieceRequests, 0, cap)
	}
	return got.([]*peersForPieceRequests)[:0]
}

type peersForPieceSorter struct {
	peersForPiece []*peersForPieceRequests
	req           *RequestIndex
	p             requestablePiece
}

func (me *peersForPieceSorter) Len() int {
	return len(me.peersForPiece)
}

func (me *peersForPieceSorter) Swap(i, j int) {
	me.peersForPiece[i], me.peersForPiece[j] = me.peersForPiece[j], me.peersForPiece[i]
}

func (me *peersForPieceSorter) Less(_i, _j int) bool {
	i := me.peersForPiece[_i]
	j := me.peersForPiece[_j]
	req := me.req
	p := &me.p
	byHasRequest := func() multiless.Computation {
		ml := multiless.New()
		if req != nil {
			iHas := i.nextState.Requests.Contains(*req)
			jHas := j.nextState.Requests.Contains(*req)
			ml = ml.Bool(jHas, iHas)
		}
		return ml
	}()
	ml := multiless.New()
	// We always "reallocate", that is force even striping amongst peers that are either on
	// the last piece they can contribute too, or for pieces marked for this behaviour.
	// Striping prevents starving peers of requests, and will always re-balance to the
	// fastest known peers.
	if !p.alwaysReallocate {
		ml = ml.Bool(
			j.requestablePiecesRemaining == 1,
			i.requestablePiecesRemaining == 1)
	}
	if p.alwaysReallocate || j.requestablePiecesRemaining == 1 {
		ml = ml.Int(
			i.requestsInPiece,
			j.requestsInPiece)
	} else {
		ml = ml.AndThen(byHasRequest)
	}
	ml = ml.Int(
		i.requestablePiecesRemaining,
		j.requestablePiecesRemaining,
	).Float64(
		j.DownloadRate,
		i.DownloadRate,
	)
	if ml.Ok() {
		return ml.Less()
	}
	ml = ml.AndThen(byHasRequest)
	return ml.Int64(
		int64(j.Age), int64(i.Age),
		// TODO: Probably peer priority can come next
	).Uintptr(
		i.Id.Uintptr(),
		j.Id.Uintptr(),
	).MustLess()
}

func allocatePendingChunks(p requestablePiece, peers []*requestsPeer) {
	peersForPiece := makePeersForPiece(len(peers))
	for _, peer := range peers {
		if !peer.canRequestPiece(p.index) {
			continue
		}
		if !peer.canFitRequest() {
			peer.requestablePiecesRemaining--
			continue
		}
		peersForPiece = append(peersForPiece, &peersForPieceRequests{
			requestsInPiece: 0,
			requestsPeer:    peer,
		})
	}
	defer func() {
		for _, peer := range peersForPiece {
			peer.requestablePiecesRemaining--
		}
		peersForPiecesPool.Put(peersForPiece)
	}()
	peersForPieceSorter := peersForPieceSorter{
		peersForPiece: peersForPiece,
		p:             p,
	}
	sortPeersForPiece := func(req *RequestIndex) {
		peersForPieceSorter.req = req
		sort.Sort(&peersForPieceSorter)
		// ensureValidSortedPeersForPieceRequests(&peersForPieceSorter)
	}
	// Chunks can be preassigned several times, if peers haven't been able to update their "actual"
	// with "next" request state before another request strategy run occurs.
	preallocated := make([][]*peersForPieceRequests, p.t.ChunksPerPiece)
	p.IterPendingChunks(func(spec ChunkIndex) {
		req := p.chunkIndexToRequestIndex(spec)
		for _, peer := range peersForPiece {
			if !peer.ExistingRequests.Contains(req) {
				continue
			}
			if !peer.canFitRequest() {
				continue
			}
			preallocated[spec] = append(preallocated[spec], peer)
			peer.addNextRequest(req)
		}
	})
	pendingChunksRemaining := int(p.NumPendingChunks)
	p.IterPendingChunks(func(chunk ChunkIndex) {
		if len(preallocated[chunk]) != 0 {
			return
		}
		req := p.chunkIndexToRequestIndex(chunk)
		defer func() { pendingChunksRemaining-- }()
		sortPeersForPiece(nil)
		for _, peer := range peersForPiece {
			if !peer.canFitRequest() {
				continue
			}
			if !peer.PieceAllowedFast.ContainsInt(p.index) {
				// TODO: Verify that's okay to stay uninterested if we request allowed fast pieces.
				peer.nextState.Interested = true
				if peer.Choking {
					continue
				}
			}
			peer.addNextRequest(req)
			break
		}
	})
chunk:
	for chunk, prePeers := range preallocated {
		if len(prePeers) == 0 {
			continue
		}
		pendingChunksRemaining--
		req := p.chunkIndexToRequestIndex(ChunkIndex(chunk))
		for _, pp := range prePeers {
			pp.requestsInPiece--
		}
		sortPeersForPiece(&req)
		for _, pp := range prePeers {
			pp.nextState.Requests.Remove(req)
		}
		for _, peer := range peersForPiece {
			if !peer.canFitRequest() {
				continue
			}
			if !peer.PieceAllowedFast.ContainsInt(p.index) {
				// TODO: Verify that's okay to stay uninterested if we request allowed fast pieces.
				peer.nextState.Interested = true
				if peer.Choking {
					continue
				}
			}
			peer.addNextRequest(req)
			continue chunk
		}
	}
	if pendingChunksRemaining != 0 {
		panic(pendingChunksRemaining)
	}
}
