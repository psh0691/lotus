package blocksync

import (
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/store"
	"time"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/chain/types"
)

var log = logging.Logger("blocksync")

const BlockSyncProtocolID = "/fil/sync/blk/0.0.1"

// FIXME: Bumped from original 800 to this to accommodate `syncFork()`
//  use of `GetBlocks()`. It seems the expectation of that API is to
//  fetch any amount of blocks leaving it to the internal logic here
//  to partition and reassemble the requests if they go above the maximum.
const MaxRequestLength = uint64(build.ForkLengthThreshold)

// Extracted constants from the code.
// FIXME: Should be reviewed and confirmed.
const SUCCESS_PEER_TAG_VALUE = 25
const WRITE_REQ_DEADLINE = 5 * time.Second
const READ_RES_DEADLINE = WRITE_REQ_DEADLINE
const READ_RES_MIN_SPEED = 50<<10
const SHUFFLE_PEERS_PREFIX = 5
const WRITE_RES_DEADLINE = 60 * time.Second

// FIXME: Rename. Make private.
type Request struct {
	// List of ordered CIDs comprising a `TipSetKey` from where to start
	// fetching backwards.
	// FIXME: Why don't we send a `TipSetKey` instead of converting back
	//  and forth?
	Head []cid.Cid
	// Number of block sets to fetch from `Head` (inclusive, should always
	// be in the range `[1, MaxRequestLength]`).
	Length uint64
	// Request options, see `Options` type for more details. Compressed
	// in a single `uint64` to save space.
	Options uint64
}

// `Request` processed and validated to query the tipsets needed.
type validatedRequest struct {
	head    types.TipSetKey
	length  uint64
	options *parsedOptions
}

// Request options. When fetching the chain segment we can fetch
// either block headers, messages, or both.
const (
	Headers = 1 << iota
	Messages
)

// Decompressed options into separate struct members for easy access
// during internal processing..
type parsedOptions struct {
	IncludeHeaders  bool
	IncludeMessages bool
}

func (options *parsedOptions) noOptionsSet() bool {
	return options.IncludeHeaders == false &&
		options.IncludeMessages == false
}

func parseOptions(optfield uint64) *parsedOptions {
	return &parsedOptions{
		IncludeHeaders:  optfield&(uint64(Headers)) != 0,
		IncludeMessages: optfield&(uint64(Messages)) != 0,
	}
}

// FIXME: Rename. Make private.
type Response struct {
	Status       status
	// String that complements the error status when converting to an
	// internal error (see `statusToError()`).
	ErrorMessage string

	Chain []*BSTipSet
}

type status uint64
const (
	Ok status = 0
	// We could not fetch all blocks requested (but at least we returned
	// the `Head` requested). Not considered an error.
	Partial = 101

	// Errors
	NotFound      = 201
	GoAway        = 202
	InternalError = 203
	BadRequest    = 204
)

// Convert status to internal error.
func (res *Response) statusToError() error {
	switch res.Status {
	case Ok, Partial:
		return nil
		// FIXME: Consider if we want to not process `Partial` responses
		//  and return an error instead.
	case NotFound:
		return xerrors.Errorf("not found")
	case GoAway:
		return xerrors.Errorf("not handling 'go away' blocksync responses yet")
	case InternalError:
		return xerrors.Errorf("block sync peer errored: %s", res.ErrorMessage)
	case BadRequest:
		return xerrors.Errorf("block sync request invalid: %s", res.ErrorMessage)
	default:
		return xerrors.Errorf("unrecognized response code: %d", res.Status)
	}
}

// FIXME: Rename.
type BSTipSet struct {
	Blocks []*types.BlockHeader
    Messages *CompactedMessages
}

// FIXME: Describe format. The `Includes` seem to index
//  from block to message.
// FIXME: The logic of this function should belong to it, not
//  to the consumer.
type CompactedMessages struct {
	Bls    []*types.Message
	BlsIncludes [][]uint64

	Secpk    []*types.SignedMessage
	SecpkIncludes [][]uint64
}

// Response that has been validated according to the protocol
// and can be safely accessed.
// FIXME: Maybe rename to verified, keep consistent naming.
type ValidatedResponse struct {
	Tipsets []*types.TipSet
	Messages []*CompactedMessages
}

// Decompress messages and form full tipsets with them. The headers
// need to have been requested as well.
func (res *ValidatedResponse) toFullTipSets() ([]*store.FullTipSet) {
	if len(res.Tipsets) == 0 {
		// This decompression can only be done if both headers and
		// messages are returned in the response.
		// FIXME: Do we need to check the messages are present also? The validation
		//  would seem to imply this is unnecessary, can be added just in case.
		return nil
	}
	ftsList := make([]*store.FullTipSet, len(res.Tipsets))
	for tipsetIdx := range res.Tipsets {
		fts := &store.FullTipSet{} // FIXME: We should use the `NewFullTipSet` API.
		msgs := res.Messages[tipsetIdx]
		for blockIdx, b := range res.Tipsets[tipsetIdx].Blocks() {
			fb := &types.FullBlock{
				Header: b,
			}
			for _, mi := range msgs.BlsIncludes[blockIdx] {
				fb.BlsMessages = append(fb.BlsMessages, msgs.Bls[mi])
			}
			for _, mi := range msgs.SecpkIncludes[blockIdx] {
				fb.SecpkMessages = append(fb.SecpkMessages, msgs.Secpk[mi])
			}

			fts.Blocks = append(fts.Blocks, fb)
		}
		ftsList[tipsetIdx] = fts
	}
	return ftsList
}