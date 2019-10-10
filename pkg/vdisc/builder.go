// Copyright © 2019 NVIDIA Corporation
package vdisc

import (
	"compress/gzip"
	"context"
	"io"
	stdurl "net/url"
	"path"

	"github.com/pkg/errors"
	capnp "zombiezen.com/go/capnproto2"

	"github.com/NVIDIA/vdisc/pkg/iso9660"
	"github.com/NVIDIA/vdisc/pkg/safecast"
	"github.com/NVIDIA/vdisc/pkg/storage"
	"github.com/NVIDIA/vdisc/pkg/vdisc/types"
)

// Builder is an interface for building a vdisc
type Builder interface {
	SetSystemIdentifier(string)
	SetVolumeIdentifier(string)
	SetVolumeSetIdentifier(string)
	SetPublisherIdentifier(string)
	SetDataPreparerIdentifier(string)
	SetApplicationIdentifier(string)
	SetCopyrightFileIdentifier(string)
	SetAbstractFileIdentifier(string)
	SetBibliographicFileIdentifier(string)
	AddFile(path string, url string, size int64) error
	AddSymlink(path string, target string) error
	Build() (string, error)
}

// BuilderConfig is common configuration for a Builder implementation
type BuilderConfig struct {
	URL string
}

type builder struct {
	cfg      BuilderConfig
	volume   *iso9660.Volume
	numFiles int32
}

// NewISO9660Builder returns a Builder of POSIX portable volume
func NewISO9660Builder(cfg BuilderConfig) Builder {
	return NewPosixPortableISO9660Builder(cfg)
}

// NewPosixPortableISO9660Builder returns a Builder of POSIX portable volume
func NewPosixPortableISO9660Builder(cfg BuilderConfig) Builder {
	return &builder{
		cfg:    cfg,
		volume: iso9660.NewPosixPortableVolume(),
	}
}

// NewExtendedISO9660Builder returns a Builder of NvidiaExtendedVolume
func NewExtendedISO9660Builder(cfg BuilderConfig) Builder {
	return &builder{
		cfg:    cfg,
		volume: iso9660.NewNvidiaExtendedVolume(),
	}
}

// AddFile adds a file to the builder
func (b *builder) AddFile(path string, url string, size int64) error {
	r, err := storage.OpenContextSize(context.Background(), url, size)
	if err != nil {
		return err
	}

	err = b.volume.AddFile(path, r)
	if err != nil {
		return err
	}

	b.numFiles++
	return nil
}

// AddSymlink adds a symlink to the builder
func (b *builder) AddSymlink(path string, target string) error {
	return b.volume.AddSymlink(path, target)
}

func (b *builder) SetSystemIdentifier(val string) {
	b.volume.SetSystemIdentifier(val)
}

func (b *builder) SetVolumeIdentifier(val string) {
	b.volume.SetVolumeIdentifier(val)
}

func (b *builder) SetVolumeSetIdentifier(val string) {
	b.volume.SetVolumeSetIdentifier(val)
}

func (b *builder) SetPublisherIdentifier(val string) {
	b.volume.SetPublisherIdentifier(val)
}

func (b *builder) SetDataPreparerIdentifier(val string) {
	b.volume.SetDataPreparerIdentifier(val)
}

func (b *builder) SetApplicationIdentifier(val string) {
	b.volume.SetApplicationIdentifier(val)
}

func (b *builder) SetCopyrightFileIdentifier(val string) {
	b.volume.SetCopyrightFileIdentifier(val)
}

func (b *builder) SetAbstractFileIdentifier(val string) {
	b.volume.SetAbstractFileIdentifier(val)
}

func (b *builder) SetBibliographicFileIdentifier(val string) {
	b.volume.SetBibliographicFileIdentifier(val)
}

// Build builds the volume, returning the URL
func (b *builder) Build() (string, error) {
	//
	// First, write out the iso9660 metadata to a new object
	//
	metadataURL := b.cfg.URL + ".isohdr"
	meta, err := storage.Create(metadataURL)
	if err != nil {
		return "", errors.Wrap(err, "creating "+metadataURL)
	}
	defer meta.Abort()

	metaLen, err := b.volume.WriteMetadataTo(meta)
	if err != nil {
		return "", errors.Wrap(err, "writing iso9660 metadata")
	}

	metaCommitInfo, err := meta.Commit()
	if err != nil {
		return "", errors.Wrap(err, "closing "+metadataURL)
	}

	mu, err := stdurl.Parse(metaCommitInfo.ObjectURL())
	if err != nil {
		return "", errors.Wrap(err, "parsing "+metaCommitInfo.ObjectURL())
	}

	var muBase stdurl.URL
	muBase.Path = path.Base(mu.Path)
	muBase.RawQuery = mu.RawQuery

	//
	// Then build up the inverted trie of object URLs
	//
	trie := NewTrieMap()
	trie.Put(muBase.String(), 0)
	b.volume.VisitFileInodes(func(finode *iso9660.FileInode) error {
		obj := finode.Object()
		trie.Put(obj.URL(), finode.Start())
		return nil
	})

	inverted, leaves := trie.Invert()

	//
	// Create a new vdisc object
	//
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		return "", errors.Wrap(err, "capnp.NewMessage")
	}

	vroot, err := vdisc_types.NewRootVDisc(seg)
	if err != nil {
		return "", errors.Wrap(err, "vdisc_types.NewRootVDisc")
	}

	vdisc, err := vroot.NewV1()
	if err != nil {
		return "", errors.Wrap(err, "vroot.NewV1()")
	}

	vdisc.SetBlockSize(iso9660.LogicalBlockSize)
	vdisc.SetFsType("iso9660")

	//
	// Populate the inverted trie of URIs
	//
	uris, err := vdisc.NewUris(safecast.IntToInt32(len(inverted)))
	if err != nil {
		return "", errors.Wrap(err, "vdisc.NewUris")
	}

	for i, inode := range inverted {
		node := uris.At(i)
		node.SetParent(safecast.IntToUint32(inode.Parent))
		node.SetContent(inode.Content)
	}

	//
	// Add the extents
	//
	numExtents := b.numFiles + 1
	extents, err := vdisc.NewExtents(numExtents)
	if err != nil {
		return "", errors.Wrap(err, "vdisc.NewExtents")
	}

	metaBlocks := bytesToSectors(metaLen)
	metaPadding := uint16(sectorsToBytes(metaBlocks) - metaLen)
	entry := extents.At(0)
	metaLeaf := leaves[0]
	entry.SetUriPrefix(safecast.IntToUint32(metaLeaf.Parent))
	entry.SetUriSuffix(metaLeaf.Content)

	entry.SetBlocks(metaBlocks)
	entry.SetPadding(metaPadding)

	currExtent := 1
	b.volume.VisitFileInodes(func(finode *iso9660.FileInode) error {
		obj := finode.Object()
		blocks := bytesToSectors(obj.Size())
		padding := uint16(sectorsToBytes(blocks) - obj.Size())

		entry := extents.At(currExtent)
		leaf := leaves[finode.Start()]
		entry.SetUriPrefix(safecast.IntToUint32(leaf.Parent))
		entry.SetUriSuffix(leaf.Content)
		entry.SetBlocks(blocks)
		entry.SetPadding(padding)

		currExtent++
		return nil
	})

	//
	// Finally, store the vdisc object
	//
	vd, err := storage.Create(b.cfg.URL)
	if err != nil {
		return "", errors.Wrap(err, "creating "+b.cfg.URL)
	}
	defer vd.Abort()

	var vdw io.Writer = vd
	vdz, err := gzip.NewWriterLevel(vd, gzip.BestCompression)
	if err != nil {
		return "", errors.Wrap(err, "creating gzip writer")
	}
	vdw = vdz

	if err := capnp.NewEncoder(vdw).Encode(msg); err != nil {
		return "", errors.Wrap(err, "capnp encode")
	}

	if err := vdz.Close(); err != nil {
		return "", errors.Wrap(err, "closing gzip writer")
	}

	vdiscCommitInfo, err := vd.Commit()
	if err != nil {
		return "", errors.Wrap(err, "closing "+b.cfg.URL)
	}

	return vdiscCommitInfo.ObjectURL(), nil
}

// Calculates the number of sectors needed to hold bytes. Zero bytes result in one sector.
func bytesToSectors(bytes int64) uint32 {
	sectors := uint32(bytes / iso9660.LogicalBlockSize)
	if (bytes%iso9660.LogicalBlockSize) != 0 || sectors == 0 {
		sectors++
	}
	return sectors
}

// Calculates the number of bytes occuppied by sectors
func sectorsToBytes(sectors uint32) int64 {
	return int64(sectors) * iso9660.LogicalBlockSize
}
