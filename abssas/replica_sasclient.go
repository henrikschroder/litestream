package abssas

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/internal"
	"golang.org/x/sync/errgroup"
)

// ReplicaClientType is the client type for this package.
const ReplicaClientType = "abssas"

var _ litestream.ReplicaClient = (*ReplicaClient)(nil)

// ReplicaClient is a client for writing snapshots & WAL segments to disk.
type ReplicaClient struct {
	mu           sync.Mutex
	containerURL *azblob.ContainerURL

	container string
	path      string
	sasURL    string
}

// NewReplicaClient returns a new instance of ReplicaClient.
func NewReplicaClient(sasUrl string) (*ReplicaClient, error) {

	// parse url
	u, err := url.Parse(sasUrl)
	if err != nil {
		return nil, err
	}

	// Extract container name and path from URL
	pathParts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(pathParts) < 1 {
		return nil, fmt.Errorf("invalid SAS URL format: missing container name")
	}

	// First part is the container name
	container := pathParts[0]

	// The rest is the path (if any)
	var path string
	if len(pathParts) > 1 {
		// Don't add a leading slash to the path - this is causing the "<no name>" folder
		path = strings.Join(pathParts[1:], "/")
	}

	client := &ReplicaClient{
		container: container,
		path:      path,
		sasURL:    sasUrl,
	}
	return client, nil
}

// Type returns "abs" as the client type.
func (c *ReplicaClient) Type() string {
	return ReplicaClientType
}

// Init initializes the connection to Azure. No-op if already initialized.
func (c *ReplicaClient) Init(ctx context.Context) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.containerURL != nil {
		return nil
	}

	// Parse the SAS URL
	sasURL, err := url.Parse(c.sasURL)
	if err != nil {
		return fmt.Errorf("cannot parse SAS URL: %w", err)
	}

	// Create a pipeline using anonymous credentials since auth is in the SAS token
	pipeline := azblob.NewPipeline(azblob.NewAnonymousCredential(), azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			TryTimeout: 24 * time.Hour,
		},
	})

	// We need to create a container URL without the path part
	// First, create a copy of the URL
	containerURLStr := *sasURL

	// Set the path to just the container part
	containerURLStr.Path = "/" + c.container

	// Create the container URL
	containerURL := azblob.NewContainerURL(containerURLStr, pipeline)
	c.containerURL = &containerURL

	return nil
}

// Generations returns a list of available generation names.
func (c *ReplicaClient) Generations(ctx context.Context) ([]string, error) {
	if err := c.Init(ctx); err != nil {
		return nil, err
	}

	var generations []string
	var marker azblob.Marker
	for marker.NotDone() {
		internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "LIST").Inc()

		// Use the full path including the path component after the container
		prefix := litestream.GenerationsPath(c.path) + "/"
		resp, err := c.containerURL.ListBlobsHierarchySegment(ctx, marker, "/", azblob.ListBlobsSegmentOptions{
			Prefix: prefix,
		})
		if err != nil {
			return nil, err
		}
		marker = resp.NextMarker

		for _, prefix := range resp.Segment.BlobPrefixes {
			name := path.Base(strings.TrimSuffix(prefix.Name, "/"))
			if !litestream.IsGenerationName(name) {
				continue
			}
			generations = append(generations, name)
		}
	}

	return generations, nil
}

// DeleteGeneration deletes all snapshots & WAL segments within a generation.
func (c *ReplicaClient) DeleteGeneration(ctx context.Context, generation string) error {
	if err := c.Init(ctx); err != nil {
		return err
	}

	dir, err := litestream.GenerationPath(c.path, generation)
	if err != nil {
		return fmt.Errorf("cannot determine generation path: %w", err)
	}

	var marker azblob.Marker
	for marker.NotDone() {
		internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "LIST").Inc()

		// Use the full path including the path component after the container
		prefix := dir + "/"
		resp, err := c.containerURL.ListBlobsFlatSegment(ctx, marker, azblob.ListBlobsSegmentOptions{Prefix: prefix})
		if err != nil {
			return err
		}
		marker = resp.NextMarker

		for _, item := range resp.Segment.BlobItems {
			internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "DELETE").Inc()

			blobURL := c.containerURL.NewBlobURL(item.Name)
			if _, err := blobURL.Delete(ctx, azblob.DeleteSnapshotsOptionNone, azblob.BlobAccessConditions{}); isNotExists(err) {
				continue
			} else if err != nil {
				return err
			}
		}
	}

	// log.Printf("%s(%s): retainer: deleting generation: %s", r.db.Path(), r.Name(), generation)

	return nil
}

// Snapshots returns an iterator over all available snapshots for a generation.
func (c *ReplicaClient) Snapshots(ctx context.Context, generation string) (litestream.SnapshotIterator, error) {
	if err := c.Init(ctx); err != nil {
		return nil, err
	}
	return newSnapshotIterator(ctx, generation, c), nil
}

// WriteSnapshot writes LZ4 compressed data from rd to the object storage.
func (c *ReplicaClient) WriteSnapshot(ctx context.Context, generation string, index int, rd io.Reader) (info litestream.SnapshotInfo, err error) {
	if err := c.Init(ctx); err != nil {
		return info, err
	}

	key, err := litestream.SnapshotPath(c.path, generation, index)
	if err != nil {
		return info, fmt.Errorf("cannot determine snapshot path: %w", err)
	}
	startTime := time.Now()

	rc := internal.NewReadCounter(rd)

	// Use the full path including the path component after the container
	blobURL := c.containerURL.NewBlockBlobURL(key)
	if _, err := azblob.UploadStreamToBlockBlob(ctx, rc, blobURL, azblob.UploadStreamToBlockBlobOptions{
		BlobHTTPHeaders: azblob.BlobHTTPHeaders{ContentType: "application/octet-stream"},
		BlobAccessTier:  azblob.DefaultAccessTier,
	}); err != nil {
		return info, err
	}

	internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "PUT").Inc()
	internal.OperationBytesCounterVec.WithLabelValues(ReplicaClientType, "PUT").Add(float64(rc.N()))

	// log.Printf("%s(%s): snapshot: creating %s/%08x t=%s", r.db.Path(), r.Name(), generation, index, time.Since(startTime).Truncate(time.Millisecond))

	return litestream.SnapshotInfo{
		Generation: generation,
		Index:      index,
		Size:       rc.N(),
		CreatedAt:  startTime.UTC(),
	}, nil
}

// SnapshotReader returns a reader for snapshot data at the given generation/index.
func (c *ReplicaClient) SnapshotReader(ctx context.Context, generation string, index int) (io.ReadCloser, error) {
	if err := c.Init(ctx); err != nil {
		return nil, err
	}

	key, err := litestream.SnapshotPath(c.path, generation, index)
	if err != nil {
		return nil, fmt.Errorf("cannot determine snapshot path: %w", err)
	}

	blobURL := c.containerURL.NewBlobURL(key)
	resp, err := blobURL.Download(ctx, 0, 0, azblob.BlobAccessConditions{}, false, azblob.ClientProvidedKeyOptions{})
	if isNotExists(err) {
		return nil, os.ErrNotExist
	} else if err != nil {
		return nil, fmt.Errorf("cannot start new reader for %q: %w", key, err)
	}

	internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "GET").Inc()
	internal.OperationBytesCounterVec.WithLabelValues(ReplicaClientType, "GET").Add(float64(resp.ContentLength()))

	return resp.Body(azblob.RetryReaderOptions{}), nil
}

// DeleteSnapshot deletes a snapshot with the given generation & index.
func (c *ReplicaClient) DeleteSnapshot(ctx context.Context, generation string, index int) error {
	if err := c.Init(ctx); err != nil {
		return err
	}

	key, err := litestream.SnapshotPath(c.path, generation, index)
	if err != nil {
		return fmt.Errorf("cannot determine snapshot path: %w", err)
	}

	internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "DELETE").Inc()

	blobURL := c.containerURL.NewBlobURL(key)
	if _, err := blobURL.Delete(ctx, azblob.DeleteSnapshotsOptionNone, azblob.BlobAccessConditions{}); isNotExists(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("cannot delete snapshot %q: %w", key, err)
	}
	return nil
}

// WALSegments returns an iterator over all available WAL files for a generation.
func (c *ReplicaClient) WALSegments(ctx context.Context, generation string) (litestream.WALSegmentIterator, error) {
	if err := c.Init(ctx); err != nil {
		return nil, err
	}
	return newWALSegmentIterator(ctx, generation, c), nil
}

// WriteWALSegment writes LZ4 compressed data from rd into a file on disk.
func (c *ReplicaClient) WriteWALSegment(ctx context.Context, pos litestream.Pos, rd io.Reader) (info litestream.WALSegmentInfo, err error) {
	if err := c.Init(ctx); err != nil {
		return info, err
	}

	key, err := litestream.WALSegmentPath(c.path, pos.Generation, pos.Index, pos.Offset)
	if err != nil {
		return info, fmt.Errorf("cannot determine wal segment path: %w", err)
	}
	startTime := time.Now()

	rc := internal.NewReadCounter(rd)

	blobURL := c.containerURL.NewBlockBlobURL(key)
	if _, err := azblob.UploadStreamToBlockBlob(ctx, rc, blobURL, azblob.UploadStreamToBlockBlobOptions{
		BlobHTTPHeaders: azblob.BlobHTTPHeaders{ContentType: "application/octet-stream"},
		BlobAccessTier:  azblob.DefaultAccessTier,
	}); err != nil {
		return info, err
	}

	internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "PUT").Inc()
	internal.OperationBytesCounterVec.WithLabelValues(ReplicaClientType, "PUT").Add(float64(rc.N()))

	return litestream.WALSegmentInfo{
		Generation: pos.Generation,
		Index:      pos.Index,
		Offset:     pos.Offset,
		Size:       rc.N(),
		CreatedAt:  startTime.UTC(),
	}, nil
}

// WALSegmentReader returns a reader for a section of WAL data at the given index.
// Returns os.ErrNotExist if no matching index/offset is found.
func (c *ReplicaClient) WALSegmentReader(ctx context.Context, pos litestream.Pos) (io.ReadCloser, error) {
	if err := c.Init(ctx); err != nil {
		return nil, err
	}

	key, err := litestream.WALSegmentPath(c.path, pos.Generation, pos.Index, pos.Offset)
	if err != nil {
		return nil, fmt.Errorf("cannot determine wal segment path: %w", err)
	}

	blobURL := c.containerURL.NewBlobURL(key)
	resp, err := blobURL.Download(ctx, 0, 0, azblob.BlobAccessConditions{}, false, azblob.ClientProvidedKeyOptions{})
	if isNotExists(err) {
		return nil, os.ErrNotExist
	} else if err != nil {
		return nil, fmt.Errorf("cannot start new reader for %q: %w", key, err)
	}

	internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "GET").Inc()
	internal.OperationBytesCounterVec.WithLabelValues(ReplicaClientType, "GET").Add(float64(resp.ContentLength()))

	return resp.Body(azblob.RetryReaderOptions{}), nil
}

// DeleteWALSegments deletes WAL segments with at the given positions.
func (c *ReplicaClient) DeleteWALSegments(ctx context.Context, a []litestream.Pos) error {
	if err := c.Init(ctx); err != nil {
		return err
	}

	for _, pos := range a {
		key, err := litestream.WALSegmentPath(c.path, pos.Generation, pos.Index, pos.Offset)
		if err != nil {
			return fmt.Errorf("cannot determine wal segment path: %w", err)
		}

		internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "DELETE").Inc()

		blobURL := c.containerURL.NewBlobURL(key)
		if _, err := blobURL.Delete(ctx, azblob.DeleteSnapshotsOptionNone, azblob.BlobAccessConditions{}); isNotExists(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("cannot delete wal segment %q: %w", key, err)
		}
	}

	return nil
}

type snapshotIterator struct {
	client     *ReplicaClient
	generation string

	ch     chan litestream.SnapshotInfo
	g      errgroup.Group
	ctx    context.Context
	cancel func()

	info litestream.SnapshotInfo
	err  error
}

func newSnapshotIterator(ctx context.Context, generation string, client *ReplicaClient) *snapshotIterator {
	itr := &snapshotIterator{
		client:     client,
		generation: generation,
		ch:         make(chan litestream.SnapshotInfo),
	}

	itr.ctx, itr.cancel = context.WithCancel(ctx)
	itr.g.Go(itr.fetch)

	return itr
}

// fetch runs in a separate goroutine to fetch pages of objects and stream them to a channel.
func (itr *snapshotIterator) fetch() error {
	defer close(itr.ch)

	dir, err := litestream.SnapshotsPath(itr.client.path, itr.generation)
	if err != nil {
		return fmt.Errorf("cannot determine snapshots path: %w", err)
	}

	var marker azblob.Marker
	for marker.NotDone() {
		internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "LIST").Inc()

		// Use the full path including the path component after the container
		prefix := dir + "/"
		resp, err := itr.client.containerURL.ListBlobsFlatSegment(itr.ctx, marker, azblob.ListBlobsSegmentOptions{Prefix: prefix})
		if err != nil {
			return err
		}
		marker = resp.NextMarker

		for _, item := range resp.Segment.BlobItems {
			key := path.Base(item.Name)
			index, err := litestream.ParseSnapshotPath(key)
			if err != nil {
				continue
			}

			info := litestream.SnapshotInfo{
				Generation: itr.generation,
				Index:      index,
				Size:       *item.Properties.ContentLength,
				CreatedAt:  item.Properties.CreationTime.UTC(),
			}

			select {
			case <-itr.ctx.Done():
			case itr.ch <- info:
			}
		}
	}
	return nil
}

func (itr *snapshotIterator) Close() (err error) {
	err = itr.err

	// Cancel context and wait for error group to finish.
	itr.cancel()
	if e := itr.g.Wait(); e != nil && err == nil {
		err = e
	}

	return err
}

func (itr *snapshotIterator) Next() bool {
	// Exit if an error has already occurred.
	if itr.err != nil {
		return false
	}

	// Return false if context was canceled or if there are no more snapshots.
	// Otherwise fetch the next snapshot and store it on the iterator.
	select {
	case <-itr.ctx.Done():
		return false
	case info, ok := <-itr.ch:
		if !ok {
			return false
		}
		itr.info = info
		return true
	}
}

func (itr *snapshotIterator) Err() error { return itr.err }

func (itr *snapshotIterator) Snapshot() litestream.SnapshotInfo {
	return itr.info
}

type walSegmentIterator struct {
	client     *ReplicaClient
	generation string

	ch     chan litestream.WALSegmentInfo
	g      errgroup.Group
	ctx    context.Context
	cancel func()

	info litestream.WALSegmentInfo
	err  error
}

func newWALSegmentIterator(ctx context.Context, generation string, client *ReplicaClient) *walSegmentIterator {
	itr := &walSegmentIterator{
		client:     client,
		generation: generation,
		ch:         make(chan litestream.WALSegmentInfo),
	}

	itr.ctx, itr.cancel = context.WithCancel(ctx)
	itr.g.Go(itr.fetch)

	return itr
}

// fetch runs in a separate goroutine to fetch pages of objects and stream them to a channel.
func (itr *walSegmentIterator) fetch() error {
	defer close(itr.ch)

	dir, err := litestream.WALPath(itr.client.path, itr.generation)
	if err != nil {
		return fmt.Errorf("cannot determine wal path: %w", err)
	}

	var marker azblob.Marker
	for marker.NotDone() {
		internal.OperationTotalCounterVec.WithLabelValues(ReplicaClientType, "LIST").Inc()

		// Use the full path including the path component after the container
		prefix := dir + "/"
		resp, err := itr.client.containerURL.ListBlobsFlatSegment(itr.ctx, marker, azblob.ListBlobsSegmentOptions{Prefix: prefix})
		if err != nil {
			return err
		}
		marker = resp.NextMarker

		for _, item := range resp.Segment.BlobItems {
			key := path.Base(item.Name)
			index, offset, err := litestream.ParseWALSegmentPath(key)
			if err != nil {
				continue
			}

			info := litestream.WALSegmentInfo{
				Generation: itr.generation,
				Index:      index,
				Offset:     offset,
				Size:       *item.Properties.ContentLength,
				CreatedAt:  item.Properties.CreationTime.UTC(),
			}

			select {
			case <-itr.ctx.Done():
			case itr.ch <- info:
			}
		}
	}
	return nil
}

func (itr *walSegmentIterator) Close() (err error) {
	err = itr.err

	// Cancel context and wait for error group to finish.
	itr.cancel()
	if e := itr.g.Wait(); e != nil && err == nil {
		err = e
	}

	return err
}

func (itr *walSegmentIterator) Next() bool {
	// Exit if an error has already occurred.
	if itr.err != nil {
		return false
	}

	// Return false if context was canceled or if there are no more segments.
	// Otherwise fetch the next segment and store it on the iterator.
	select {
	case <-itr.ctx.Done():
		return false
	case info, ok := <-itr.ch:
		if !ok {
			return false
		}
		itr.info = info
		return true
	}
}

func (itr *walSegmentIterator) Err() error { return itr.err }

func (itr *walSegmentIterator) WALSegment() litestream.WALSegmentInfo {
	return itr.info
}

func isNotExists(err error) bool {
	switch err := err.(type) {
	case azblob.StorageError:
		return err.ServiceCode() == azblob.ServiceCodeBlobNotFound
	default:
		return false
	}
}
