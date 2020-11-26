package rocks

import (
	"context"
	"time"
)

// Basic Types

// DiffType represents a changed state for a given entry (added, removed, changed, conflict)
type DiffType uint8

//goland:noinspection GoUnusedConst
const (
	DiffTypeAdded DiffType = iota
	DiffTypeRemoved
	DiffTypeChanged
	DiffTypeConflict
)

// function/methods receiving the following basic types could assume they passed validation
type (
	// StorageNamespace is the URI to the storage location
	StorageNamespace string

	// RepositoryID is an identifier for a repo
	RepositoryID string

	// Path represents a logical path for an entry
	Path string

	// Ref could be a commit ID, a branch name, a Tag
	Ref string

	// TagID represents a named tag pointing at a commit
	TagID string

	// CommitID is a content addressable hash representing a Commit object
	CommitID string

	// BranchID is an identifier for a branch
	BranchID string

	// TreeID represents a snapshot of the tree, referenced by a commit
	TreeID string

	// StagingToken represents a namespace for writes to apply as uncommitted
	StagingToken string

	// CommonPrefix represents a path prefixing one or more Entry objects
	CommonPrefix string
)

// Repository represents repository metadata
type Repository struct {
	StorageNamespace StorageNamespace
	CreationDate     time.Time
	DefaultBranchID  BranchID
}

type RepositoryRecord struct {
	RepositoryID RepositoryID
	*Repository
}

// Entry represents metadata or a given object (modified date, physical address, etc)
type Entry struct {
	LastModified time.Time
	Address      string
	Metadata     map[string]string
	ETag         string
}

func (e *Entry) IsTombstone() bool {
	return e == nil
}

// EntryRecord holds Path with the associated Entry information
type EntryRecord struct {
	Path Path
	*Entry
}

// Commit represents commit metadata (author, time, tree ID)
type Commit struct {
	Committer    string
	Message      string
	TreeID       TreeID
	CreationDate time.Time
	Parents      []CommitID
	Metadata     map[string]string
}

// CommitRecords holds CommitID with the associated Commit data
type CommitRecord struct {
	CommitID CommitID
	*Commit
}

// Branch is a pointer to a commit
type Branch struct {
	CommitID CommitID
	// nolint: structcheck, unused
	stagingToken StagingToken
}

// BranchRecord holds BranchID with the associated Branch data
type BranchRecord struct {
	BranchID BranchID
	*Branch
}

// Listing represents either an entry or a CommonPrefix
type Listing struct {
	CommonPrefix
	*Entry
}

// Diff represents a change in path
type Diff struct {
	Path Path
	Type DiffType
}

// Interfaces
type Catalog interface {
	// entries
	GetEntry(ctx context.Context, repositoryID RepositoryID, ref Ref, path Path) (*Entry, error)
	SetEntry(ctx context.Context, repositoryID RepositoryID, branchID BranchID, path Path, entry Entry) error
	DeleteEntry(ctx context.Context, repositoryID RepositoryID, branchID BranchID, path Path) error
	ListEntries(ctx context.Context, repositoryID RepositoryID, ref Ref, prefix, from, delimiter string, amount int) ([]Listing, bool, error)

	// refs
	CreateBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID, ref Ref) (Branch, error)
	GetBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID) (Branch, error)
	Dereference(ctx context.Context, repositoryID RepositoryID, ref Ref) (CommitID, error)
	Log(ctx context.Context, repositoryID RepositoryID, commitID CommitID, amount int) ([]Commit, bool, error)
	ListBranches(ctx context.Context, repositoryID RepositoryID, from BranchID, amount int) ([]Branch, bool, error)
	DeleteBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID) error

	// commits
	Commit(ctx context.Context, repositoryID RepositoryID, branchID BranchID, commit Commit) (CommitID, error)
	Reset(ctx context.Context, repositoryID RepositoryID, branchID BranchID) error
	Revert(ctx context.Context, repositoryID RepositoryID, branchID BranchID, ref Ref) error

	// diffs and merges
	Merge(ctx context.Context, repositoryID RepositoryID, from Ref, to BranchID) (CommitID, error)
	DiffUncommitted(ctx context.Context, repositoryID RepositoryID, branchID BranchID, from Path, amount int) ([]Diff, bool, error)
	Diff(ctx context.Context, repositoryID RepositoryID, left, right Ref, from Path, amount int) ([]Diff, bool, error)
}

// Internal structures used by Catalog
// xxxIterator used as follow:
// ```
// it := NewXXXIterator(data)
// for it.Next() {
//    data := it.Value()
//    process(data)
// }
// if it.Err() {
//   return fmt.Errorf("stopped because of an error %w", it.Err())
// }
// ```
// Calling SeekGE() returns true, like calling Next() - we can process 'Value()' when true and check Err() in case of false
// When Next() or SeekGE() returns false (doesn't matter if it because of an error) calling Value() should return nil

type RepositoryIterator interface {
	Next() bool
	SeekGE(id RepositoryID) bool
	Value() *RepositoryRecord
	Err() error
	Close()
}

type EntryIterator interface {
	Next() bool
	SeekGE(id Path) bool
	Value() *EntryRecord
	Err() error
	Close()
}

type DiffIterator interface {
	Next() bool
	SeekGE(id Path) bool
	Value() *Diff
	Err() error
	Close()
}

type BranchIterator interface {
	Next() bool
	SeekGE(id BranchID) bool
	Value() *BranchRecord
	Err() error
	Close()
}

type CommitIterator interface {
	Next() bool
	SeekGE(id CommitID) bool
	Value() *CommitRecord
	Err() error
	Close()
}

// These are the more complex internal components that compose the functionality of the Catalog

// RefManager handles references: branches, commits, probably tags in the future
// it also handles the structure of the commit graph and its traversal (notably, merge-base and log)
type RefManager interface {
	// GetRepository returns the Repository metadata object for the given RepositoryID
	GetRepository(ctx context.Context, repositoryID RepositoryID) (*Repository, error)

	// CreateRepository stores a new Repository under RepositoryID with the given Branch as default branch
	CreateRepository(ctx context.Context, repositoryID RepositoryID, repository Repository, branch Branch) error

	// ListRepositories lists repositories
	ListRepositories(ctx context.Context, from RepositoryID) (RepositoryIterator, error)

	// DeleteRepository deletes the repository
	DeleteRepository(ctx context.Context, repositoryID RepositoryID) error

	// Dereference translates Ref to the corresponding CommitID
	Dereference(ctx context.Context, repositoryID RepositoryID, ref Ref) (CommitID, error)

	// GetBranch returns the Branch metadata object for the given BranchID
	GetBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID) (*Branch, error)

	// SetBranch points the given BranchID at the given Branch metadata
	SetBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID, branch Branch) error

	// DeleteBranch deletes the branch
	DeleteBranch(ctx context.Context, repositoryID RepositoryID, branchID BranchID) error

	// ListBranches lists branches
	ListBranches(ctx context.Context, repositoryID RepositoryID, from BranchID) (BranchIterator, error)

	// GetCommit returns the Commit metadata object for the given CommitID
	GetCommit(ctx context.Context, repositoryID RepositoryID, commitID CommitID) (*Commit, error)

	// AddCommit stores the Commit object, returning its ID
	AddCommit(ctx context.Context, repositoryID RepositoryID, commit Commit) (CommitID, error)

	// FindMergeBase returns the merge-base for the given CommitIDs
	// see: https://git-scm.com/docs/git-merge-base
	// and internally: https://github.com/treeverse/lakeFS/blob/09954804baeb36ada74fa17d8fdc13a38552394e/index/dag/commits.go
	FindMergeBase(ctx context.Context, repositoryID RepositoryID, commitIDs ...CommitID) (*Commit, error)

	// Log returns an iterator that reads all parents up to the first commit
	Log(ctx context.Context, repositoryID RepositoryID, from CommitID) (CommitIterator, error)
}

// CommittedManager reads and applies committed snapshots
// it is responsible for de-duping them, persisting them and providing basic diff, merge and list capabilities
type CommittedManager interface {
	// GetEntry returns the provided path, if exists, from the provided TreeID
	GetEntry(ctx context.Context, ns StorageNamespace, treeID TreeID, path Path) (*Entry, error)

	// ListEntries takes a given tree and returns an EntryIterator seeked to >= "from" path
	ListEntries(ctx context.Context, ns StorageNamespace, treeID TreeID, from Path) (EntryIterator, error)

	// Diff receives two trees and a 3rd merge base tree used to resolve the change type
	// it tracks changes from left to right, returning an iterator of Diff entries
	Diff(ctx context.Context, ns StorageNamespace, left, right, base TreeID, from Path) (DiffIterator, error)

	// Merge receives two trees and a 3rd merge base tree used to resolve the change type
	// it applies that changes from left to right, resulting in a new tree that
	// is expected to be immediately addressable
	Merge(ctx context.Context, ns StorageNamespace, left, right, base TreeID) (TreeID, error)

	// Apply is the act of taking an existing tree (snapshot) and applying a set of changes to it.
	// A change is either an entity to write/overwrite, or a tombstone to mark a deletion
	// it returns a new treeID that is expected to be immediately addressable
	Apply(ctx context.Context, ns StorageNamespace, treeID TreeID, entryIterator EntryIterator) (TreeID, error)
}

// StagingManager handles changes to a branch that aren't yet committed
// provides basic CRUD abilities, with deletes being written as tombstones (null entry)
type StagingManager interface {
	// GetEntry returns the provided path (or nil entry to represent a tombstone)
	//   Returns ErrNotFound if no entry found on path
	GetEntry(ctx context.Context, repositoryID RepositoryID, branchID BranchID, st StagingToken, from Path) (*Entry, error)

	// SetEntry writes an entry (or nil entry to represent a tombstone)
	SetEntry(ctx context.Context, repositoryID RepositoryID, branchID BranchID, path Path, entry *Entry) error

	// DeleteEntry deletes an entry by path
	DeleteEntry(ctx context.Context, repositoryID RepositoryID, branchID BranchID, path Path) error

	// ListEntries takes a given BranchID and returns an EntryIterator seeked to >= "from" path
	ListEntries(ctx context.Context, repositoryID RepositoryID, branchID BranchID, st StagingToken, from Path) (EntryIterator, error)

	// Snapshot returns a new snapshot and returns it's ID
	Snapshot(ctx context.Context, repositoryID RepositoryID, branchID BranchID, st StagingToken) (StagingToken, error)

	// ListSnapshot returns an iterator to scan the snapshot entries
	ListSnapshot(ctx context.Context, repositoryID RepositoryID, branchID BranchID, st StagingToken, from Path) (EntryIterator, error)
}