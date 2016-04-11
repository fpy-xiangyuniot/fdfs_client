package fdfs_client

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"bytes"
)

type Client struct {
	trackerPools	map[string]*ConnPool
	storagePools	map[string]*ConnPool
	storagePoolLock *sync.RWMutex
	config			*Config
}

func NewClientWithConfig(configName string) (*Client, error) {
	config, err := NewConfig(configName)
	if err != nil {
		return nil, err
    }
	client := &Client{
		config:					config,
		storagePoolLock:		&sync.RWMutex{},
	}
	client.trackerPools = make(map[string]*ConnPool)
	client.storagePools = make(map[string]*ConnPool)

	for _, addr := range config.trackerAddr {
		trackerPool, err := NewConnPool(addr, config.maxConns)
		if err != nil {
			return nil, err
		}
		client.trackerPools[addr] = trackerPool
    }

	return client, nil
}

func (this *Client) Destory() {
	for _, pool := range this.trackerPools {
		pool.Destory()
    }
	for _, pool := range this.storagePools {
		pool.Destory()
    }
}

func (this *Client) UploadByFilename(fileName string) (*FileId, error) {
	fileInfo, err := this.checkFileInfo(fileName)
	defer func() {
		if fileInfo != nil && fileInfo.file != nil{
			fileInfo.file.Close()
        }
	}()
	if err != nil {
		return nil, err
	}

	storageInfo, err := this.queryStorageInfoWithTracker(TRACKER_PROTO_CMD_SERVICE_QUERY_STORE_WITHOUT_GROUP_ONE,"","")
	if err != nil {
		return nil, err
	}

	return this.uploadFileToStorage(fileInfo, storageInfo)
}

func (this *Client) DownloadByFileId(fileId string,localFilename string) error {
	groupName, remoteFilename, err := SplitFileId(fileId)
	if err != nil {
		return err
	}
	storageInfo, err := this.queryStorageInfoWithTracker(TRACKER_PROTO_CMD_SERVICE_QUERY_FETCH_ONE,groupName,remoteFilename)
	if err != nil {
		return err
	}

	return this.downloadFileFromStorage(storageInfo,groupName,remoteFilename, localFilename,0,0)
}

func (this *Client) downloadFileFromStorage(storageInfo *StorageInfo,groupName string,remoteFilename string,localFilename string,offset int64,downloadBytes int64) error {
	storageConn, err := this.getStorageConn(storageInfo)
	if err != nil {
		return err
	}
	defer storageConn.Close()

	task := &StorageDownloadTask{}
	err = task.SendHeader(storageConn, groupName, remoteFilename, offset, downloadBytes)
	if err != nil {
		return err
	}
	if err := task.RecvFile(storageConn, localFilename);err != nil{
		return err
	}

	return nil
}

func (this *Client) queryStorageInfoWithTracker(cmd int8,groupName string,remoteFilename string) (*StorageInfo, error) {
	task := &TrackerTask{}
	if groupName != "" {
		task.pkgLen = int64(FDFS_GROUP_NAME_MAX_LEN + len(remoteFilename))
    }
	task.cmd = cmd
	
	trackerConn, err := this.getTrackerConn()
	if err != nil {
		return nil, err
	}
	defer trackerConn.Close()

	if err := task.SendHeader(trackerConn); err != nil {
		return nil, err
	}
	if groupName != "" {
		buffer := new(bytes.Buffer)
		byteGroupName := []byte(groupName)
		var bufferGroupName [16]byte
		for i := 0; i < len(byteGroupName);i++ {
			bufferGroupName[i] = byteGroupName[i]
		}
		buffer.Write(bufferGroupName[:])
		buffer.WriteString(remoteFilename)
		if _, err := trackerConn.Write(buffer.Bytes()); err != nil {
			return nil, err
		}
    }
	if err := task.RecvHeader(trackerConn); err != nil {
		return nil, err
	}

	if task.status != 0 {
		return nil, fmt.Errorf("tracker task status %v != 0", task.status)
	}
	if err := task.RecvStorageInfo(trackerConn,int(task.pkgLen)); err != nil {
		return nil, err
	}
	return &StorageInfo{
		addr:             fmt.Sprintf("%s:%d", task.ipAddr, task.port),
		storagePathIndex: task.storePathIndex,
	}, nil
}

func (this *Client) checkFileInfo(fileName string) (*FileInfo, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if int(stat.Size()) == 0 {
		return nil, fmt.Errorf("file %q size is zero", fileName)
	}
	var fileExtName string
	index := strings.LastIndexByte(fileName, '.')
	if index != -1 {
		fileExtName = fileName[index+1:]
		if len(fileExtName) > 6 {
			fileExtName = fileExtName[:6]
		}
	}
	return &FileInfo{
		fileSize:    stat.Size(),
		file:        file,
		fileExtName: fileExtName,
	}, nil
}

func (this *Client) uploadFileToStorage(fileInfo *FileInfo, storageInfo *StorageInfo) (*FileId, error) {
	storageConn, err := this.getStorageConn(storageInfo)
	if err != nil {
		return nil, err
	}
	defer storageConn.Close()

	task := &StorageUploadTask{}
	err = task.SendHeader(storageConn, fileInfo, storageInfo.storagePathIndex)
	if err != nil {
		return nil, err
	}
	err = task.SendFile(storageConn, fileInfo)
	if err != nil {
		return nil, err
	}
	return task.RecvFileId(storageConn)
}

func (this *Client) getTrackerConn() (net.Conn, error) {
	var trackerConn net.Conn
	var err error
	var getOne bool
	for _, trackerPool := range this.trackerPools {
		trackerConn, err = trackerPool.get()
		if err == nil {
			getOne = true
			break
		}
	}
	if getOne {
		return trackerConn, nil
	}
	if err == nil {
		return nil, fmt.Errorf("no connPool can be use")
	}
	return nil, err
}

func (this *Client) getStorageConn(storageInfo *StorageInfo) (net.Conn, error) {
	this.storagePoolLock.Lock()
	storagePool, ok := this.storagePools[storageInfo.addr]
	if ok {
		this.storagePoolLock.Unlock()
		return storagePool.get()
	}
	storagePool, err := NewConnPool(storageInfo.addr,this.config.maxConns)
	if err != nil {
		this.storagePoolLock.Unlock()
		return nil, err
	}
	this.storagePools[storageInfo.addr] = storagePool
	this.storagePoolLock.Unlock()
	return storagePool.get()
}