package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/treeverse/lakefs/block"

	"github.com/Azure/azure-storage-blob-go/azblob"

	guuid "github.com/google/uuid"
)

// This code is taken from azblob chunkwriting.go
// The reason is that the original code commit the data at the end of the copy
// In order to support multipart upload we need to save the blockIDs instead of committing them
// And once complete multipart is called we commit all the blockIDs

// blockWriter provides methods to upload blocks that represent a file to a server and commit them.
// This allows us to provide a local implementation that fakes the server for hermetic testing.
type blockWriter interface {
	StageBlock(context.Context, string, io.ReadSeeker, azblob.LeaseAccessConditions, []byte, azblob.ClientProvidedKeyOptions) (*azblob.BlockBlobStageBlockResponse, error)
	CommitBlockList(context.Context, []string, azblob.BlobHTTPHeaders, azblob.Metadata, azblob.BlobAccessConditions, azblob.AccessTierType, azblob.BlobTagsMap, azblob.ClientProvidedKeyOptions) (*azblob.BlockBlobCommitBlockListResponse, error)
}

func defaults(u *azblob.UploadStreamToBlockBlobOptions) error {
	if u.TransferManager != nil {
		return nil
	}

	if u.MaxBuffers == 0 {
		u.MaxBuffers = 1
	}

	if u.BufferSize < _1MiB {
		u.BufferSize = _1MiB
	}

	var err error
	u.TransferManager, err = azblob.NewStaticBuffer(u.BufferSize, u.MaxBuffers)
	if err != nil {
		return fmt.Errorf("bug: default transfer manager could not be created: %s", err)
	}
	return nil
}

// copyFromReader copies a source io.Reader to blob storage using concurrent uploads.
// TODO(someone): The existing model provides a buffer size and buffer limit as limiting factors.  The buffer size is probably
// useless other than needing to be above some number, as the network stack is going to hack up the buffer over some size. The
// max buffers is providing a cap on how much memory we use (by multiplying it times the buffer size) and how many go routines can upload
// at a time.  I think having a single max memory dial would be more efficient.  We can choose an internal buffer size that works
// well, 4 MiB or 8 MiB, and autoscale to as many goroutines within the memory limit. This gives a single dial to tweak and we can
// choose a max value for the memory setting based on internal transfers within Azure (which will give us the maximum throughput model).
// We can even provide a utility to dial this number in for customer networks to optimize their copies.
func copyFromReader(ctx context.Context, from io.Reader, to blockWriter, toIDs blockWriter, toSizes blockWriter, o azblob.UploadStreamToBlockBlobOptions) (string, error) {
	if err := defaults(&o); err != nil {
		return "", err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cp := &copier{
		ctx:     ctx,
		cancel:  cancel,
		reader:  block.NewHashingReader(from, block.HashFunctionMD5),
		to:      to,
		toIDs:   toIDs,
		toSizes: toSizes,
		id:      newID(),
		o:       o,
		errCh:   make(chan error, 1),
	}

	// Send all our chunks until we get an error.
	var err error
	for {
		if err = cp.sendChunk(); err != nil {
			break
		}
	}
	// If the error is not EOF, then we have a problem.
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}

	// Close out our upload.
	if err := cp.close(); err != nil {
		return "", err
	}

	// This part was added
	// Instead of committing in close ( what is originally done in azblob/chunkwriting.go)
	// we stage The blockIDs and size of this copy operation in the relevant blockBlobURLs ( cp.toIDs, cp.toSizes)
	// Later on, in complete multipart upload we commit the file blockBlobURLs with etags as the blockIDs
	// Then by reading the files we get the relevant blockIDs and sizes
	etag := "\"" + hex.EncodeToString(cp.reader.Md5.Sum(nil)) + "\""
	base64Etag := base64.StdEncoding.EncodeToString([]byte(etag))

	// write to blockIDs
	pd := strings.Join(cp.id.issued(), "\n") + "\n"
	_, err = cp.toIDs.StageBlock(cp.ctx, base64Etag, strings.NewReader(pd), cp.o.AccessConditions.LeaseAccessConditions, nil, cp.o.ClientProvidedKeyOptions)
	if err != nil {
		return "", fmt.Errorf("failed staging part data: %w", err)
	}
	// write block sizes
	sd := strconv.Itoa(int(cp.reader.CopiedSize)) + "\n"
	_, err = cp.toSizes.StageBlock(cp.ctx, base64Etag, strings.NewReader(sd), cp.o.AccessConditions.LeaseAccessConditions, nil, cp.o.ClientProvidedKeyOptions)
	if err != nil {
		return "", fmt.Errorf("failed staging part data: %w", err)
	}
	return etag, nil
}

// copier streams a file via chunks in parallel from a reader representing a file.
// Do not use directly, instead use copyFromReader().
type copier struct {
	// ctx holds the context of a copier. This is normally a faux pas to store a Context in a struct. In this case,
	// the copier has the lifetime of a function call, so its fine.
	ctx    context.Context
	cancel context.CancelFunc

	// o contains our options for uploading.
	o azblob.UploadStreamToBlockBlobOptions

	// id provides the ids for each chunk.
	id *id

	// reader is the source to be written to storage.
	reader *block.HashingReader
	// to is the location we are writing our chunks to.
	to      blockWriter
	toIDs   blockWriter
	toSizes blockWriter

	// errCh is used to hold the first error from our concurrent writers.
	errCh chan error
	// wg provides a count of how many writers we are waiting to finish.
	wg sync.WaitGroup

	// result holds the final result from blob storage after we have submitted all chunks.
	result *azblob.BlockBlobCommitBlockListResponse
}

type copierChunk struct {
	buffer []byte
	id     string
}

// getErr returns an error by priority. First, if a function set an error, it returns that error. Next, if the Context has an error
// it returns that error. Otherwise it is nil. getErr supports only returning an error once per copier.
func (c *copier) getErr() error {
	select {
	case err := <-c.errCh:
		return err
	default:
	}
	return c.ctx.Err()
}

// sendChunk reads data from out internal reader, creates a chunk, and sends it to be written via a channel.
// sendChunk returns io.EOF when the reader returns an io.EOF or io.ErrUnexpectedEOF.
func (c *copier) sendChunk() error {
	if err := c.getErr(); err != nil {
		return err
	}

	buffer := c.o.TransferManager.Get()
	if len(buffer) == 0 {
		return errors.New("TransferManager returned a 0 size buffer, this is a bug in the manager")
	}
	n, err := io.ReadFull(c.reader, buffer)
	switch {
	case err == nil && n == 0:
		return nil
	case err == nil:
		id := c.id.next()
		c.wg.Add(1)
		c.o.TransferManager.Run(
			func() {
				defer c.wg.Done()
				c.write(copierChunk{buffer: buffer[0:n], id: id})
			},
		)
		return nil
	case err != nil && (err == io.EOF || err == io.ErrUnexpectedEOF) && n == 0:
		return io.EOF
	}

	if err == io.EOF || err == io.ErrUnexpectedEOF {
		id := c.id.next()
		c.wg.Add(1)
		c.o.TransferManager.Run(
			func() {
				defer c.wg.Done()
				c.write(copierChunk{buffer: buffer[0:n], id: id})
			},
		)
		return io.EOF
	}
	if err := c.getErr(); err != nil {
		return err
	}
	return err
}

// write uploads a chunk to blob storage.
func (c *copier) write(chunk copierChunk) {
	defer c.o.TransferManager.Put(chunk.buffer)

	if err := c.ctx.Err(); err != nil {
		return
	}
	_, err := c.to.StageBlock(c.ctx, chunk.id, bytes.NewReader(chunk.buffer), c.o.AccessConditions.LeaseAccessConditions, nil, c.o.ClientProvidedKeyOptions)
	if err != nil {
		c.errCh <- fmt.Errorf("write error: %w", err)
		return
	}
	return
}

// close commits our blocks to blob storage and closes our writer.
func (c *copier) close() error {
	c.wg.Wait()

	if err := c.getErr(); err != nil {
		return err
	}

	var err error
	return err
}

// id allows the creation of unique IDs based on UUID4 + an int32. This auto-increments.
type id struct {
	u   [64]byte
	num uint32
	all []string
}

// newID constructs a new id.
func newID() *id {
	uu := guuid.New()
	u := [64]byte{}
	copy(u[:], uu[:])
	return &id{u: u}
}

// next returns the next ID.
func (id *id) next() string {
	defer atomic.AddUint32(&id.num, 1)

	binary.BigEndian.PutUint32((id.u[len(guuid.UUID{}):]), atomic.LoadUint32(&id.num))
	str := base64.StdEncoding.EncodeToString(id.u[:])
	id.all = append(id.all, str)

	return str
}

// issued returns all ids that have been issued. This returned value shares the internal slice so it is not safe to modify the return.
// The value is only valid until the next time next() is called.
func (id *id) issued() []string {
	return id.all
}