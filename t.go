package torrent

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/chansync/events"
	"github.com/anacrolix/missinggo/v2/pubsub"
	"github.com/anacrolix/sync"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const (
	downloadReqAddress = "http://127.0.0.1:1337/download"
	// TODO: hard coded for now but change to tracker addr later(?)
)

// The Torrent's infohash. This is fixed and cannot change. It uniquely identifies a torrent.
func (t *Torrent) InfoHash() metainfo.Hash {
	return t.infoHash
}

// Returns a channel that is closed when the info (.Info()) for the torrent has become available.
func (t *Torrent) GotInfo() events.Done {
	return t.gotMetainfoC
}

// Returns the metainfo info dictionary, or nil if it's not yet available.
func (t *Torrent) Info() (info *metainfo.Info) {
	t.nameMu.RLock()
	info = t.info
	t.nameMu.RUnlock()
	return
}

// Returns a Reader bound to the torrent's data. All read calls block until the data requested is
// actually available. Note that you probably want to ensure the Torrent Info is available first.
func (t *Torrent) NewReader() Reader {
	return t.newReader(0, t.length())
}

func (t *Torrent) newReader(offset, length int64) Reader {
	r := reader{
		mu:     t.cl.locker(),
		t:      t,
		offset: offset,
		length: length,
	}
	r.readaheadFunc = defaultReadaheadFunc
	t.addReader(&r)
	return &r
}

type PieceStateRuns []PieceStateRun

func (me PieceStateRuns) String() (s string) {
	if len(me) > 0 {
		var sb strings.Builder
		sb.WriteString(me[0].String())
		for i := 1; i < len(me); i += 1 {
			sb.WriteByte(' ')
			sb.WriteString(me[i].String())
		}
		return sb.String()
	}
	return
}

// Returns the state of pieces of the torrent. They are grouped into runs of same state. The sum of
// the state run-lengths is the number of pieces in the torrent.
func (t *Torrent) PieceStateRuns() (runs PieceStateRuns) {
	t.cl.rLock()
	runs = t.pieceStateRuns()
	t.cl.rUnlock()
	return
}

func (t *Torrent) PieceState(piece pieceIndex) (ps PieceState) {
	t.cl.rLock()
	ps = t.pieceState(piece)
	t.cl.rUnlock()
	return
}

// The number of pieces in the torrent. This requires that the info has been
// obtained first.
func (t *Torrent) NumPieces() pieceIndex {
	return t.numPieces()
}

// Get missing bytes count for specific piece.
func (t *Torrent) PieceBytesMissing(piece int) int64 {
	t.cl.rLock()
	defer t.cl.rUnlock()

	return int64(t.pieces[piece].bytesLeft())
}

// Drop the torrent from the client, and close it. It's always safe to do
// this. No data corruption can, or should occur to either the torrent's data,
// or connected peers.
func (t *Torrent) Drop() {
	var wg sync.WaitGroup
	defer wg.Wait()
	t.cl.lock()
	defer t.cl.unlock()
	t.cl.dropTorrent(t.infoHash, &wg)
}

// Number of bytes of the entire torrent we have completed. This is the sum of
// completed pieces, and dirtied chunks of incomplete pieces. Do not use this
// for download rate, as it can go down when pieces are lost or fail checks.
// Sample Torrent.Stats.DataBytesRead for actual file data download rate.
func (t *Torrent) BytesCompleted() int64 {
	t.cl.rLock()
	defer t.cl.rUnlock()
	return t.bytesCompleted()
}

// The subscription emits as (int) the index of pieces as their state changes.
// A state change is when the PieceState for a piece alters in value.
func (t *Torrent) SubscribePieceStateChanges() *pubsub.Subscription[PieceStateChange] {
	return t.pieceStateChanges.Subscribe()
}

// Returns true if the torrent is currently being seeded. This occurs when the
// client is willing to upload without wanting anything in return.
func (t *Torrent) Seeding() (ret bool) {
	t.cl.rLock()
	ret = t.seeding()
	t.cl.rUnlock()
	return
}

// Clobbers the torrent display name if metainfo is unavailable.
// The display name is used as the torrent name while the metainfo is unavailable.
func (t *Torrent) SetDisplayName(dn string) {
	t.nameMu.Lock()
	if !t.haveInfo() {
		t.displayName = dn
	}
	t.nameMu.Unlock()
}

// The current working name for the torrent. Either the name in the info dict,
// or a display name given such as by the dn value in a magnet link, or "".
func (t *Torrent) Name() string {
	return t.name()
}

// The completed length of all the torrent data, in all its files. This is
// derived from the torrent info, when it is available.
func (t *Torrent) Length() int64 {
	return t._length.Value
}

// Returns a run-time generated metainfo for the torrent that includes the
// info bytes and announce-list as currently known to the client.
func (t *Torrent) Metainfo() metainfo.MetaInfo {
	t.cl.rLock()
	defer t.cl.rUnlock()
	return t.newMetaInfo()
}

func (t *Torrent) addReader(r *reader) {
	t.cl.lock()
	defer t.cl.unlock()
	if t.readers == nil {
		t.readers = make(map[*reader]struct{})
	}
	t.readers[r] = struct{}{}
	r.posChanged()
}

func (t *Torrent) deleteReader(r *reader) {
	delete(t.readers, r)
	t.readersChanged()
}

// Raise the priorities of pieces in the range [begin, end) to at least Normal
// priority. Piece indexes are not the same as bytes. Requires that the info
// has been obtained, see Torrent.Info and Torrent.GotInfo.
func (t *Torrent) DownloadPieces(begin, end pieceIndex) {
	t.cl.lock()
	t.downloadPiecesLocked(begin, end)
	t.cl.unlock()
}

func (t *Torrent) downloadPiecesLocked(begin, end pieceIndex) {
	for i := begin; i < end; i++ {
		if t.pieces[i].priority.Raise(PiecePriorityNormal) {
			t.updatePiecePriority(i, "Torrent.DownloadPieces")
		}
	}
}

func (t *Torrent) CancelPieces(begin, end pieceIndex) {
	t.cl.lock()
	t.cancelPiecesLocked(begin, end, "Torrent.CancelPieces")
	t.cl.unlock()
}

func (t *Torrent) cancelPiecesLocked(begin, end pieceIndex, reason string) {
	for i := begin; i < end; i++ {
		p := &t.pieces[i]
		if p.priority == PiecePriorityNone {
			continue
		}
		p.priority = PiecePriorityNone
		t.updatePiecePriority(i, reason)
	}
}

func (t *Torrent) initFiles() {
	var offset int64
	t.files = new([]*File)
	for _, fi := range t.info.UpvertedFiles() {
		*t.files = append(*t.files, &File{
			t,
			strings.Join(append([]string{t.info.BestName()}, fi.BestPath()...), "/"),
			offset,
			fi.Length,
			fi,
			fi.DisplayPath(t.info),
			PiecePriorityNone,
		})
		offset += fi.Length
	}
}

// Returns handles to the files in the torrent. This requires that the Info is
// available first.
func (t *Torrent) Files() []*File {
	return *t.files
}

func (t *Torrent) AddPeers(pp []PeerInfo) (n int) {
	t.cl.lock()
	n = t.addPeers(pp)
	t.cl.unlock()
	return
}

// Marks the entire torrent for download. Requires the info first, see
// GotInfo. Sets piece priorities for historical reasons.
func (t *Torrent) DownloadAll() {
	// TODO: update pinging
	t.DoHttpSend(Count{0})
	ticker := time.NewTicker(1 *time.Second)
	done := make (chan bool)
	go func() {
		for {
			select{
			case <- done:
				t.DoHttpSend(Count{0})
				ticker.Stop()
				return
			case currTime := <- ticker.C:
				// t.logger.Log(fmt.Sprintf("Tick at %s", currTime.String()))
				fmt.Println("Tick at ", currTime)
				if (t.checkDownloaded()) {
					done <- true 
				} else {
					t.DoHttpSend(t.stats.BytesReadData);
					// send to tracker to notify the download 
				}
			}
		}
	} ()
	t.DownloadPieces(0, t.numPieces())
	// make an extra request to set the connection to 0?? 
}

func (t *Torrent) checkDownloaded() bool {
	return t.haveInfo() && t.haveAllPieces()
}

func (t *Torrent) DoHttpSend(numBytesRead Count) int64 {
	req, err := http.NewRequest("GET", downloadReqAddress, nil)
	if err != nil {
		fmt.Println("Error during the creation of the new request")
	}
	req.Close = true
	query := req.URL.Query()
	fmt.Println("bytes downloading", numBytesRead.String())
	query.Add("downloadbytes", numBytesRead.String())
	query.Add("uploadbytes", "2000") // TODO: upload amount is hardcoded right now 
	query.Add("infohash", t.InfoHash().AsString())
	req.Header.Set("Accept-Encoding", "identity")
	req.URL.RawQuery = query.Encode()

	resp, err := t.cl.httpClient.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			fmt.Println("HTTP tracker not running")
		}
		fmt.Println("HTTP request error:", err)
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Body read error:", err)
	}

	fmt.Println(string(body))

	if len(body) == 0 {
		fmt.Println("body empty")
	}

	var decoded map[string]interface{}
	if err = bencode.Unmarshal(body, &decoded); err != nil {
		fmt.Println("Unmarshalling error:", err)
		return 0 
	}
	

	if decoded["downloadSpeed"] ==nil {
		fmt.Println("no downloadspeed")
		return 0;
	}

	downloadSpeed := decoded["downloadSpeed"].(int64) 
	if downloadSpeed <=0 {
		fmt.Println("download speed error", decoded["downloadSpeed"])
	}
	return downloadSpeed
}

func (t *Torrent) String() string {
	s := t.name()
	if s == "" {
		return t.infoHash.HexString()
	} else {
		return strconv.Quote(s)
	}
}

func (t *Torrent) AddTrackers(announceList [][]string) {
	t.cl.lock()
	defer t.cl.unlock()
	t.addTrackers(announceList)
}

func (t *Torrent) Piece(i pieceIndex) *Piece {
	return t.piece(i)
}

func (t *Torrent) PeerConns() []*PeerConn {
	t.cl.rLock()
	defer t.cl.rUnlock()
	ret := make([]*PeerConn, 0, len(t.conns))
	for c := range t.conns {
		ret = append(ret, c)
	}
	return ret
}
