package rtmp

import (
	"fmt"
	"github.com/q191201771/lal/pkg/util/log"
	"sync"
	"time"
)

type GroupObserver interface {
	AVMsgObserver
}

type Group struct {
	appName    string
	streamName string

	pubSession      *PubSession
	pullSession     *PullSession
	subSessionSet   map[*SubSession]struct{}
	prevAudioHeader *Header
	prevVideoHeader *Header

	// TODO chef:
	metadata        []byte
	avcKeySeqHeader []byte
	aacSeqHeader    []byte

	mutex sync.Mutex

	obs GroupObserver
}

func NewGroup(appName string, streamName string) *Group {
	return &Group{
		appName:       appName,
		streamName:    streamName,
		subSessionSet: make(map[*SubSession]struct{}),
	}
}

func (group *Group) RunLoop() {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			//noop
		}
	}
}

func (group *Group) Dispose() {

}

func (group *Group) AddPubSession(session *PubSession) {
	log.Debugf("add PubSession into group. [%s]", session.UniqueKey)
	group.mutex.Lock()
	group.pubSession = session
	group.mutex.Unlock()
	session.SetPubSessionObserver(group)
}

func (group *Group) AddSubSession(session *SubSession) {
	log.Debugf("add SubSession into group. [%s]", session.UniqueKey)
	group.mutex.Lock()
	group.subSessionSet[session] = struct{}{}
	group.mutex.Unlock()

	// TODO chef: 多长没有拉流session存在的功能
	//group.turnToEmptyTick = 0
}

func (group *Group) DelPubSession(session *PubSession) {
	log.Debugf("del PubSession from group. [%s]", session.UniqueKey)
	group.mutex.Lock()
	group.pubSession = nil
	group.mutex.Unlock()

}

func (group *Group) DelSubSession(session *SubSession) {
	log.Debugf("del SubSession from group. [%s]", session.UniqueKey)
	group.mutex.Lock()
	delete(group.subSessionSet, session)
	group.mutex.Unlock()
}

func (group *Group) Pull(addr string, connectTimeout int64) {
	group.pullSession = NewPullSession(group, connectTimeout)

	defer func() {
		group.mutex.Lock()
		defer group.mutex.Unlock()
		group.pullSession = nil
		log.Infof("del rtmp PullSession out of group.")
	}()

	url := fmt.Sprintf("rtmp://%s/%s/%s", addr, group.appName, group.streamName)
	if err := group.pullSession.Pull(url); err != nil {
		log.Error(err)
	}
	if err := group.pullSession.WaitLoop(); err != nil {
		log.Debugf("rtmp PullSession loop done. [%s] err=%v", group.pullSession.UniqueKey, err)
		return
	}
}

func (group *Group) IsTotalEmpty() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.pubSession == nil && len(group.subSessionSet) == 0
}

func (group *Group) IsInExist() bool {
	group.mutex.Lock()
	defer group.mutex.Unlock()
	return group.pubSession != nil
}

func (group *Group) SetObserver(obs GroupObserver) {
	group.obs = obs
}

// PubSession or PullSession
func (group *Group) ReadRTMPAVMsgCB(header Header, timestampAbs int, message []byte) {
	group.mutex.Lock()
	defer group.mutex.Unlock()

	group.broadcastRTMP2RTMP(header, timestampAbs, message)

	if group.obs != nil {
		group.obs.ReadRTMPAVMsgCB(header, timestampAbs, message)
	}
}

func (group *Group) broadcastRTMP2RTMP(header Header, timestampAbs int, message []byte) {
	//log.Infof("%+v", header)
	var currHeader Header
	currHeader.MsgLen = len(message)
	currHeader.Timestamp = timestampAbs
	currHeader.MsgTypeID = header.MsgTypeID
	currHeader.MsgStreamID = MSID1
	//var prevHeader *Header

	switch header.MsgTypeID {
	case TypeidDataMessageAMF0:
		currHeader.CSID = CSIDAMF
		//prevHeader = nil
	case TypeidAudio:
		currHeader.CSID = CSIDAudio
		//prevHeader = group.prevAudioHeader
	case TypeidVideo:
		currHeader.CSID = CSIDVideo
		//prevHeader = group.prevVideoHeader
	}

	// TODO chef: 所有都使用abs格式了
	var absChunks []byte

	for session := range group.subSessionSet {
		if absChunks == nil {
			absChunks = Message2Chunks(message, &currHeader, nil, LocalChunkSize)
		}

		// 是新连接
		if session.isFresh {
			// 发送缓存的头部信息
			if group.metadata != nil {
				session.AsyncWrite(group.metadata)
			}
			if group.avcKeySeqHeader != nil {
				session.AsyncWrite(group.avcKeySeqHeader)
			}
			if group.aacSeqHeader != nil {
				session.AsyncWrite(group.aacSeqHeader)
			}

			session.isFresh = false

		} else {
			// 首次发送，从I帧开始
			if session.waitKeyNalu {
				if header.MsgTypeID == TypeidDataMessageAMF0 {
					session.AsyncWrite(absChunks)
				} else if header.MsgTypeID == TypeidAudio {
					if (message[0]>>4) == 0x0a && message[1] == 0x0 {
						session.AsyncWrite(absChunks)
					}
				} else if header.MsgTypeID == TypeidVideo {
					if message[0] == 0x17 && message[1] == 0x0 {
						session.AsyncWrite(absChunks)
					}
					if message[0] == 0x17 && message[1] == 0x1 {
						session.AsyncWrite(absChunks)
						session.waitKeyNalu = false
					}
				}
			} else {
				session.AsyncWrite(absChunks)
			}
		}

	}

	switch header.MsgTypeID {
	case TypeidDataMessageAMF0:
		if absChunks == nil {
			absChunks = Message2Chunks(message, &currHeader, nil, LocalChunkSize)
		}
		log.Debug("cache metadata.")
		group.metadata = absChunks
	case TypeidVideo:
		// TODO chef: magic number
		if message[0] == 0x17 && message[1] == 0x0 {
			if absChunks == nil {
				absChunks = Message2Chunks(message, &currHeader, nil, LocalChunkSize)
			}
			log.Debug("cache avc key seq header.")
			group.avcKeySeqHeader = absChunks
		}
	case TypeidAudio:
		if (message[0]>>4) == 0x0a && message[1] == 0x0 {
			if absChunks == nil {
				absChunks = Message2Chunks(message, &currHeader, nil, LocalChunkSize)
			}
			log.Debug("cache aac seq header.")
			group.aacSeqHeader = absChunks
		}
	}
}