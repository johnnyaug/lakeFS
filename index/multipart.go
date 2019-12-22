package index

import (
	"crypto/rand"
	"strings"
	"time"
	"treeverse-lake/ident"
	"treeverse-lake/index/errors"
	"treeverse-lake/index/merkle"
	"treeverse-lake/index/model"
	"treeverse-lake/index/store"
)

const (
	MaxPartsInMultipartUpload = 10000
	MinPartInMultipartUpload  = 1
)

type MultipartManager interface {
	Create(repoId, path string, createTime time.Time) (uploadId string, err error)
	UploadPart(repoId, path, uploadId string, partNumber int, blob *model.Blob, uploadTime time.Time) error
	CopyPart(repoId, path, uploadId string, partNumber int, sourcePath, sourceBranch string, uploadTime time.Time) error
	Abort(repoId, uploadId string) error
	Complete(repoId, branch, path, uploadId string, completionTime time.Time) error
}

type KVMultipartManager struct {
	kv store.Store
}

func NewKVMultipartManager(kv store.Store) *KVMultipartManager {
	return &KVMultipartManager{kv}
}

func (m *KVMultipartManager) generateId() (string, error) {
	b := make([]byte, 1024*256) // generate a random 256k slice of bytes
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return ident.Bytes(b), nil
}

func (m *KVMultipartManager) Create(repoId, path string, createTime time.Time) (string, error) {
	uploadId, err := m.kv.RepoTransact(repoId, func(tx store.RepoOperations) (interface{}, error) {

		// generate 256KB of random bytes
		uploadId, err := m.generateId()
		if err != nil {
			return uploadId, err
		}

		// save it for this repo and path
		err = tx.WriteMultipartUpload(&model.MultipartUpload{
			Path:      path,
			Id:        uploadId,
			Timestamp: createTime.Unix(),
		})
		return uploadId, err
	})
	return uploadId.(string), err
}

func (m *KVMultipartManager) UploadPart(repoId, path, uploadId string, partNumber int, part *model.MultipartUploadPart, uploadTime time.Time) error {
	_, err := m.kv.RepoTransact(repoId, func(tx store.RepoOperations) (interface{}, error) {
		// verify upload and part number
		mpu, err := tx.ReadMultipartUpload(uploadId)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(mpu.GetPath(), path) {
			return nil, errors.ErrMultipartPathMismatch
		}
		// validate part number is 1-10000
		if partNumber < MinPartInMultipartUpload || partNumber >= MaxPartsInMultipartUpload {
			return nil, errors.ErrMultipartInvalidPartNumber
		}
		err = tx.WriteMultipartUploadPart(uploadId, partNumber, part)
		return nil, err
	})
	return err
}

func (m *KVMultipartManager) CopyPart(repoId, path, uploadId string, partNumber int, sourcePath, sourceBranch string, uploadTime time.Time) error {
	_, err := m.kv.RepoTransact(repoId, func(tx store.RepoOperations) (interface{}, error) {
		// verify upload and part number
		mpu, err := tx.ReadMultipartUpload(uploadId)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(mpu.GetPath(), path) {
			return nil, errors.ErrMultipartPathMismatch
		}
		// validate part number is 1-10000
		if partNumber < MinPartInMultipartUpload || partNumber >= MaxPartsInMultipartUpload {
			return nil, errors.ErrMultipartInvalidPartNumber
		}
		// read source branch and addr
		branch, err := tx.ReadBranch(sourceBranch)
		if err != nil {
			return nil, err
		}

		// read root tree and traverse to path
		m := merkle.New(branch.GetCommitRoot())
		obj, err := m.GetObject(tx, sourcePath)
		if err != nil {
			return nil, err
		}

		// copy it as MPU part
		err = tx.WriteMultipartUploadPart(uploadId, partNumber, &model.MultipartUploadPart{
			Blob:      obj.GetBlob(),
			Timestamp: uploadTime.Unix(),
			Size:      obj.GetSize(),
		})
		return nil, err
	})
	return err
}

func (m *KVMultipartManager) Abort(repoId, uploadId string) error {
	_, err := m.kv.RepoTransact(repoId, func(tx store.RepoOperations) (interface{}, error) {
		// read it first
		mpu, err := tx.ReadMultipartUpload(uploadId)
		if err != nil {
			return nil, err
		}
		// delete all part references
		err = tx.DeleteMultipartUploadParts(uploadId)
		if err != nil {
			return nil, err
		}
		// delete mpu ID
		err = tx.DeleteMultipartUpload(uploadId, mpu.GetPath())
		return nil, err

	})
	return err
}

func (m *KVMultipartManager) Complete(repoId, branch, path, uploadId string, completionTime time.Time) error {
	_, err := m.kv.RepoTransact(repoId, func(tx store.RepoOperations) (interface{}, error) {
		var err error

		// create new object in the current workspace for the given branch
		upload, err := tx.ReadMultipartUpload(uploadId)
		if err != nil {
			return nil, err
		}

		// TODO: iterate all parts and compose object consisting of their super blob
		var size int64
		blocks := make([]*model.Block, 0)

		parts, err := tx.ListMultipartUploadParts(uploadId)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			for _, block := range part.Blob.GetBlocks() {
				blocks = append(blocks, block)
			}
			size += part.GetSize()
		}

		// build object
		obj := &model.Object{
			Blob:      &model.Blob{Blocks: blocks},
			Timestamp: completionTime.Unix(),
			Size:      size,
		}

		err = tx.WriteToWorkspacePath(branch, upload.GetPath(), &model.WorkspaceEntry{
			Path: upload.GetPath(),
			Data: &model.WorkspaceEntry_Object{Object: obj},
		})
		if err != nil {
			return nil, err
		}

		// remove MPU entry
		err = tx.DeleteMultipartUploadParts(uploadId)
		if err != nil {
			return nil, err
		}

		// remove MPU part entries for the MPU
		err = tx.DeleteMultipartUpload(uploadId, upload.GetPath())
		return nil, err
	})
	return err
}