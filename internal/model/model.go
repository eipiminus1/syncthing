// Copyright (C) 2014 Jakob Borg and Contributors (see the CONTRIBUTORS file).
// All rights reserved. Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package model

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/internal/config"
	"github.com/syncthing/syncthing/internal/events"
	"github.com/syncthing/syncthing/internal/files"
	"github.com/syncthing/syncthing/internal/ignore"
	"github.com/syncthing/syncthing/internal/lamport"
	"github.com/syncthing/syncthing/internal/osutil"
	"github.com/syncthing/syncthing/internal/protocol"
	"github.com/syncthing/syncthing/internal/scanner"
	"github.com/syncthing/syncthing/internal/stats"
	"github.com/syncthing/syncthing/internal/versioner"
	"github.com/syndtr/goleveldb/leveldb"
)

type repoState int

const (
	RepoIdle repoState = iota
	RepoScanning
	RepoSyncing
	RepoCleaning
)

func (s repoState) String() string {
	switch s {
	case RepoIdle:
		return "idle"
	case RepoScanning:
		return "scanning"
	case RepoCleaning:
		return "cleaning"
	case RepoSyncing:
		return "syncing"
	default:
		return "unknown"
	}
}

// How many files to send in each Index/IndexUpdate message.
const (
	indexTargetSize   = 250 * 1024 // Aim for making index messages no larger than 250 KiB (uncompressed)
	indexPerFileSize  = 250        // Each FileInfo is approximately this big, in bytes, excluding BlockInfos
	IndexPerBlockSize = 40         // Each BlockInfo is approximately this big
	indexBatchSize    = 1000       // Either way, don't include more files than this
)

type Model struct {
	indexDir string
	cfg      *config.Configuration
	db       *leveldb.DB

	nodeName      string
	clientName    string
	clientVersion string

	repoCfgs     map[string]config.RepositoryConfiguration          // repo -> cfg
	repoFiles    map[string]*files.Set                              // repo -> files
	repoNodes    map[string][]protocol.NodeID                       // repo -> nodeIDs
	nodeRepos    map[protocol.NodeID][]string                       // nodeID -> repos
	nodeStatRefs map[protocol.NodeID]*stats.NodeStatisticsReference // nodeID -> statsRef
	repoIgnores  map[string]ignore.Patterns                         // repo -> list of ignore patterns
	rmut         sync.RWMutex                                       // protects the above

	repoState        map[string]repoState // repo -> state
	repoStateChanged map[string]time.Time // repo -> time when state changed
	smut             sync.RWMutex

	protoConn map[protocol.NodeID]protocol.Connection
	rawConn   map[protocol.NodeID]io.Closer
	nodeVer   map[protocol.NodeID]string
	pmut      sync.RWMutex // protects protoConn and rawConn

	addedRepo bool
	started   bool
}

var (
	ErrNoSuchFile = errors.New("no such file")
	ErrInvalid    = errors.New("file is invalid")
)

// NewModel creates and starts a new model. The model starts in read-only mode,
// where it sends index information to connected peers and responds to requests
// for file data without altering the local repository in any way.
func NewModel(indexDir string, cfg *config.Configuration, nodeName, clientName, clientVersion string, db *leveldb.DB) *Model {
	m := &Model{
		indexDir:         indexDir,
		cfg:              cfg,
		db:               db,
		nodeName:         nodeName,
		clientName:       clientName,
		clientVersion:    clientVersion,
		repoCfgs:         make(map[string]config.RepositoryConfiguration),
		repoFiles:        make(map[string]*files.Set),
		repoNodes:        make(map[string][]protocol.NodeID),
		nodeRepos:        make(map[protocol.NodeID][]string),
		nodeStatRefs:     make(map[protocol.NodeID]*stats.NodeStatisticsReference),
		repoIgnores:      make(map[string]ignore.Patterns),
		repoState:        make(map[string]repoState),
		repoStateChanged: make(map[string]time.Time),
		protoConn:        make(map[protocol.NodeID]protocol.Connection),
		rawConn:          make(map[protocol.NodeID]io.Closer),
		nodeVer:          make(map[protocol.NodeID]string),
	}

	var timeout = 20 * 60 // seconds
	if t := os.Getenv("STDEADLOCKTIMEOUT"); len(t) > 0 {
		it, err := strconv.Atoi(t)
		if err == nil {
			timeout = it
		}
	}
	deadlockDetect(&m.rmut, time.Duration(timeout)*time.Second)
	deadlockDetect(&m.smut, time.Duration(timeout)*time.Second)
	deadlockDetect(&m.pmut, time.Duration(timeout)*time.Second)
	return m
}

// StartRW starts read/write processing on the current model. When in
// read/write mode the model will attempt to keep in sync with the cluster by
// pulling needed files from peer nodes.
func (m *Model) StartRepoRW(repo string) {
	m.rmut.Lock()
	cfg, ok := m.repoCfgs[repo]
	m.rmut.Unlock()

	if !ok {
		panic("cannot start nonexistent repo " + repo)
	}

	p := Puller{
		repo:     repo,
		dir:      cfg.Directory,
		scanIntv: time.Duration(cfg.RescanIntervalS) * time.Second,
		model:    m,
	}

	if len(cfg.Versioning.Type) > 0 {
		factory, ok := versioner.Factories[cfg.Versioning.Type]
		if !ok {
			l.Fatalf("Requested versioning type %q that does not exist", cfg.Versioning.Type)
		}
		p.versioner = factory(repo, cfg.Directory, cfg.Versioning.Params)
	}

	go p.Serve()
}

// StartRO starts read only processing on the current model. When in
// read only mode the model will announce files to the cluster but not
// pull in any external changes.
func (m *Model) StartRepoRO(repo string) {
	intv := time.Duration(m.repoCfgs[repo].RescanIntervalS) * time.Second
	go func() {
		for {
			time.Sleep(intv)

			if debug {
				l.Debugln(m, "rescan", repo)
			}

			m.setState(repo, RepoScanning)
			if err := m.ScanRepo(repo); err != nil {
				invalidateRepo(m.cfg, repo, err)
				return
			}
			m.setState(repo, RepoIdle)
		}
	}()
}

type ConnectionInfo struct {
	protocol.Statistics
	Address       string
	ClientVersion string
}

// ConnectionStats returns a map with connection statistics for each connected node.
func (m *Model) ConnectionStats() map[string]ConnectionInfo {
	type remoteAddrer interface {
		RemoteAddr() net.Addr
	}

	m.pmut.RLock()
	m.rmut.RLock()

	var res = make(map[string]ConnectionInfo)
	for node, conn := range m.protoConn {
		ci := ConnectionInfo{
			Statistics:    conn.Statistics(),
			ClientVersion: m.nodeVer[node],
		}
		if nc, ok := m.rawConn[node].(remoteAddrer); ok {
			ci.Address = nc.RemoteAddr().String()
		}

		res[node.String()] = ci
	}

	m.rmut.RUnlock()
	m.pmut.RUnlock()

	in, out := protocol.TotalInOut()
	res["total"] = ConnectionInfo{
		Statistics: protocol.Statistics{
			At:            time.Now(),
			InBytesTotal:  in,
			OutBytesTotal: out,
		},
	}

	return res
}

// Returns statistics about each node
func (m *Model) NodeStatistics() map[string]stats.NodeStatistics {
	var res = make(map[string]stats.NodeStatistics)
	for _, node := range m.cfg.Nodes {
		res[node.NodeID.String()] = m.nodeStatRef(node.NodeID).GetStatistics()
	}
	return res
}

// Returns the completion status, in percent, for the given node and repo.
func (m *Model) Completion(node protocol.NodeID, repo string) float64 {
	var tot int64

	m.rmut.RLock()
	rf, ok := m.repoFiles[repo]
	m.rmut.RUnlock()
	if !ok {
		return 0 // Repo doesn't exist, so we hardly have any of it
	}

	rf.WithGlobalTruncated(func(f protocol.FileIntf) bool {
		if !f.IsDeleted() {
			tot += f.Size()
		}
		return true
	})

	if tot == 0 {
		return 100 // Repo is empty, so we have all of it
	}

	var need int64
	rf.WithNeedTruncated(node, func(f protocol.FileIntf) bool {
		if !f.IsDeleted() {
			need += f.Size()
		}
		return true
	})

	res := 100 * (1 - float64(need)/float64(tot))
	if debug {
		l.Debugf("%v Completion(%s, %q): %f (%d / %d)", m, node, repo, res, need, tot)
	}

	return res
}

func sizeOf(fs []protocol.FileInfo) (files, deleted int, bytes int64) {
	for _, f := range fs {
		fs, de, by := sizeOfFile(f)
		files += fs
		deleted += de
		bytes += by
	}
	return
}

func sizeOfFile(f protocol.FileIntf) (files, deleted int, bytes int64) {
	if !f.IsDeleted() {
		files++
	} else {
		deleted++
	}
	bytes += f.Size()
	return
}

// GlobalSize returns the number of files, deleted files and total bytes for all
// files in the global model.
func (m *Model) GlobalSize(repo string) (files, deleted int, bytes int64) {
	m.rmut.RLock()
	defer m.rmut.RUnlock()
	if rf, ok := m.repoFiles[repo]; ok {
		rf.WithGlobalTruncated(func(f protocol.FileIntf) bool {
			fs, de, by := sizeOfFile(f)
			files += fs
			deleted += de
			bytes += by
			return true
		})
	}
	return
}

// LocalSize returns the number of files, deleted files and total bytes for all
// files in the local repository.
func (m *Model) LocalSize(repo string) (files, deleted int, bytes int64) {
	m.rmut.RLock()
	defer m.rmut.RUnlock()
	if rf, ok := m.repoFiles[repo]; ok {
		rf.WithHaveTruncated(protocol.LocalNodeID, func(f protocol.FileIntf) bool {
			if f.IsInvalid() {
				return true
			}
			fs, de, by := sizeOfFile(f)
			files += fs
			deleted += de
			bytes += by
			return true
		})
	}
	return
}

// NeedSize returns the number and total size of currently needed files.
func (m *Model) NeedSize(repo string) (files int, bytes int64) {
	m.rmut.RLock()
	defer m.rmut.RUnlock()
	if rf, ok := m.repoFiles[repo]; ok {
		rf.WithNeedTruncated(protocol.LocalNodeID, func(f protocol.FileIntf) bool {
			fs, de, by := sizeOfFile(f)
			files += fs + de
			bytes += by
			return true
		})
	}
	if debug {
		l.Debugf("%v NeedSize(%q): %d %d", m, repo, files, bytes)
	}
	return
}

// NeedFiles returns the list of currently needed files, stopping at maxFiles
// files or maxBlocks blocks. Limits <= 0 are ignored.
func (m *Model) NeedFilesRepoLimited(repo string, maxFiles, maxBlocks int) []protocol.FileInfo {
	m.rmut.RLock()
	defer m.rmut.RUnlock()
	nblocks := 0
	if rf, ok := m.repoFiles[repo]; ok {
		fs := make([]protocol.FileInfo, 0, maxFiles)
		rf.WithNeed(protocol.LocalNodeID, func(f protocol.FileIntf) bool {
			fi := f.(protocol.FileInfo)
			fs = append(fs, fi)
			nblocks += len(fi.Blocks)
			return (maxFiles <= 0 || len(fs) < maxFiles) && (maxBlocks <= 0 || nblocks < maxBlocks)
		})
		return fs
	}
	return nil
}

// Index is called when a new node is connected and we receive their full index.
// Implements the protocol.Model interface.
func (m *Model) Index(nodeID protocol.NodeID, repo string, fs []protocol.FileInfo) {
	if debug {
		l.Debugf("IDX(in): %s %q: %d files", nodeID, repo, len(fs))
	}

	if !m.repoSharedWith(repo, nodeID) {
		events.Default.Log(events.RepoRejected, map[string]string{
			"repo": repo,
			"node": nodeID.String(),
		})
		l.Warnf("Unexpected repository ID %q sent from node %q; ensure that the repository exists and that this node is selected under \"Share With\" in the repository configuration.", repo, nodeID)
		return
	}

	m.rmut.RLock()
	files, ok := m.repoFiles[repo]
	ignores, _ := m.repoIgnores[repo]
	m.rmut.RUnlock()

	if !ok {
		l.Fatalf("Index for nonexistant repo %q", repo)
	}

	for i := 0; i < len(fs); {
		lamport.Default.Tick(fs[i].Version)
		if ignores.Match(fs[i].Name) {
			fs[i] = fs[len(fs)-1]
			fs = fs[:len(fs)-1]
		} else {
			i++
		}
	}

	files.Replace(nodeID, fs)

	events.Default.Log(events.RemoteIndexUpdated, map[string]interface{}{
		"node":    nodeID.String(),
		"repo":    repo,
		"items":   len(fs),
		"version": files.LocalVersion(nodeID),
	})
}

// IndexUpdate is called for incremental updates to connected nodes' indexes.
// Implements the protocol.Model interface.
func (m *Model) IndexUpdate(nodeID protocol.NodeID, repo string, fs []protocol.FileInfo) {
	if debug {
		l.Debugf("%v IDXUP(in): %s / %q: %d files", m, nodeID, repo, len(fs))
	}

	if !m.repoSharedWith(repo, nodeID) {
		l.Infof("Update for unexpected repository ID %q sent from node %q; ensure that the repository exists and that this node is selected under \"Share With\" in the repository configuration.", repo, nodeID)
		return
	}

	m.rmut.RLock()
	files, ok := m.repoFiles[repo]
	ignores, _ := m.repoIgnores[repo]
	m.rmut.RUnlock()

	if !ok {
		l.Fatalf("IndexUpdate for nonexistant repo %q", repo)
	}

	for i := 0; i < len(fs); {
		lamport.Default.Tick(fs[i].Version)
		if ignores.Match(fs[i].Name) {
			fs[i] = fs[len(fs)-1]
			fs = fs[:len(fs)-1]
		} else {
			i++
		}
	}

	files.Update(nodeID, fs)

	events.Default.Log(events.RemoteIndexUpdated, map[string]interface{}{
		"node":    nodeID.String(),
		"repo":    repo,
		"items":   len(fs),
		"version": files.LocalVersion(nodeID),
	})
}

func (m *Model) repoSharedWith(repo string, nodeID protocol.NodeID) bool {
	m.rmut.RLock()
	defer m.rmut.RUnlock()
	for _, nrepo := range m.nodeRepos[nodeID] {
		if nrepo == repo {
			return true
		}
	}
	return false
}

func (m *Model) ClusterConfig(nodeID protocol.NodeID, cm protocol.ClusterConfigMessage) {
	m.pmut.Lock()
	if cm.ClientName == "syncthing" {
		m.nodeVer[nodeID] = cm.ClientVersion
	} else {
		m.nodeVer[nodeID] = cm.ClientName + " " + cm.ClientVersion
	}
	m.pmut.Unlock()

	l.Infof(`Node %s client is "%s %s"`, nodeID, cm.ClientName, cm.ClientVersion)

	if name := cm.GetOption("name"); name != "" {
		l.Infof("Node %s name is %q", nodeID, name)
		node := m.cfg.GetNodeConfiguration(nodeID)
		if node != nil && node.Name == "" {
			node.Name = name
			m.cfg.Save()
		}
	}

	if m.cfg.GetNodeConfiguration(nodeID).Introducer {
		// This node is an introducer. Go through the announced lists of repos
		// and nodes and add what we are missing.

		var changed bool
		for _, repo := range cm.Repositories {
			// If we don't have this repository yet, skip it. Ideally, we'd
			// offer up something in the GUI to create the repo, but for the
			// moment we only handle repos that we already have.
			if _, ok := m.repoNodes[repo.ID]; !ok {
				continue
			}

		nextNode:
			for _, node := range repo.Nodes {
				var id protocol.NodeID
				copy(id[:], node.ID)

				if m.cfg.GetNodeConfiguration(id) == nil {
					// The node is currently unknown. Add it to the config.

					l.Infof("Adding node %v to config (vouched for by introducer %v)", id, nodeID)
					newNodeCfg := config.NodeConfiguration{
						NodeID: id,
					}

					// The introducers' introducers are also our introducers.
					if node.Flags&protocol.FlagIntroducer != 0 {
						l.Infof("Node %v is now also an introducer", id)
						newNodeCfg.Introducer = true
					}

					m.cfg.Nodes = append(m.cfg.Nodes, newNodeCfg)

					changed = true
				}

				for _, er := range m.nodeRepos[id] {
					if er == repo.ID {
						// We already share the repo with this node, so
						// nothing to do.
						continue nextNode
					}
				}

				// We don't yet share this repo with this node. Add the node
				// to sharing list of the repo.

				l.Infof("Adding node %v to share %q (vouched for by introducer %v)", id, repo.ID, nodeID)

				m.nodeRepos[id] = append(m.nodeRepos[id], repo.ID)
				m.repoNodes[repo.ID] = append(m.repoNodes[repo.ID], id)

				repoCfg := m.cfg.GetRepoConfiguration(repo.ID)
				repoCfg.Nodes = append(repoCfg.Nodes, config.RepositoryNodeConfiguration{
					NodeID: id,
				})

				changed = true
			}
		}

		if changed {
			m.cfg.Save()
		}
	}
}

// Close removes the peer from the model and closes the underlying connection if possible.
// Implements the protocol.Model interface.
func (m *Model) Close(node protocol.NodeID, err error) {
	l.Infof("Connection to %s closed: %v", node, err)
	events.Default.Log(events.NodeDisconnected, map[string]string{
		"id":    node.String(),
		"error": err.Error(),
	})

	m.pmut.Lock()
	m.rmut.RLock()
	for _, repo := range m.nodeRepos[node] {
		m.repoFiles[repo].Replace(node, nil)
	}
	m.rmut.RUnlock()

	conn, ok := m.rawConn[node]
	if ok {
		if conn, ok := conn.(*tls.Conn); ok {
			// If the underlying connection is a *tls.Conn, Close() does more
			// than it says on the tin. Specifically, it sends a TLS alert
			// message, which might block forever if the connection is dead
			// and we don't have a deadline site.
			conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		}
		conn.Close()
	}
	delete(m.protoConn, node)
	delete(m.rawConn, node)
	delete(m.nodeVer, node)
	m.pmut.Unlock()
}

// Request returns the specified data segment by reading it from local disk.
// Implements the protocol.Model interface.
func (m *Model) Request(nodeID protocol.NodeID, repo, name string, offset int64, size int) ([]byte, error) {
	// Verify that the requested file exists in the local model.
	m.rmut.RLock()
	r, ok := m.repoFiles[repo]
	m.rmut.RUnlock()

	if !ok {
		l.Warnf("Request from %s for file %s in nonexistent repo %q", nodeID, name, repo)
		return nil, ErrNoSuchFile
	}

	lf := r.Get(protocol.LocalNodeID, name)
	if protocol.IsInvalid(lf.Flags) || protocol.IsDeleted(lf.Flags) {
		if debug {
			l.Debugf("%v REQ(in): %s: %q / %q o=%d s=%d; invalid: %v", m, nodeID, repo, name, offset, size, lf)
		}
		return nil, ErrInvalid
	}

	if offset > lf.Size() {
		if debug {
			l.Debugf("%v REQ(in; nonexistent): %s: %q o=%d s=%d", m, nodeID, name, offset, size)
		}
		return nil, ErrNoSuchFile
	}

	if debug && nodeID != protocol.LocalNodeID {
		l.Debugf("%v REQ(in): %s: %q / %q o=%d s=%d", m, nodeID, repo, name, offset, size)
	}
	m.rmut.RLock()
	fn := filepath.Join(m.repoCfgs[repo].Directory, name)
	m.rmut.RUnlock()
	fd, err := os.Open(fn) // XXX: Inefficient, should cache fd?
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	buf := make([]byte, size)
	_, err = fd.ReadAt(buf, offset)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// ReplaceLocal replaces the local repository index with the given list of files.
func (m *Model) ReplaceLocal(repo string, fs []protocol.FileInfo) {
	m.rmut.RLock()
	m.repoFiles[repo].ReplaceWithDelete(protocol.LocalNodeID, fs)
	m.rmut.RUnlock()
}

func (m *Model) CurrentRepoFile(repo string, file string) protocol.FileInfo {
	m.rmut.RLock()
	f := m.repoFiles[repo].Get(protocol.LocalNodeID, file)
	m.rmut.RUnlock()
	return f
}

func (m *Model) CurrentGlobalFile(repo string, file string) protocol.FileInfo {
	m.rmut.RLock()
	f := m.repoFiles[repo].GetGlobal(file)
	m.rmut.RUnlock()
	return f
}

type cFiler struct {
	m *Model
	r string
}

// Implements scanner.CurrentFiler
func (cf cFiler) CurrentFile(file string) protocol.FileInfo {
	return cf.m.CurrentRepoFile(cf.r, file)
}

// ConnectedTo returns true if we are connected to the named node.
func (m *Model) ConnectedTo(nodeID protocol.NodeID) bool {
	m.pmut.RLock()
	_, ok := m.protoConn[nodeID]
	m.pmut.RUnlock()
	if ok {
		m.nodeWasSeen(nodeID)
	}
	return ok
}

func (m *Model) GetIgnores(repo string) ([]string, error) {
	var lines []string

	cfg, ok := m.repoCfgs[repo]
	if !ok {
		return lines, fmt.Errorf("Repo %s does not exist", repo)
	}

	m.rmut.Lock()
	defer m.rmut.Unlock()

	fd, err := os.Open(filepath.Join(cfg.Directory, ".stignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return lines, nil
		}
		l.Warnln("Loading .stignore:", err)
		return lines, err
	}
	defer fd.Close()

	scanner := bufio.NewScanner(fd)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSpace(scanner.Text()))
	}

	return lines, nil
}

func (m *Model) SetIgnores(repo string, content []string) error {
	cfg, ok := m.repoCfgs[repo]
	if !ok {
		return fmt.Errorf("Repo %s does not exist", repo)
	}

	fd, err := ioutil.TempFile(cfg.Directory, ".syncthing.stignore-"+repo)
	if err != nil {
		l.Warnln("Saving .stignore:", err)
		return err
	}
	defer os.Remove(fd.Name())

	for _, line := range content {
		_, err = fmt.Fprintln(fd, line)
		if err != nil {
			l.Warnln("Saving .stignore:", err)
			return err
		}
	}

	err = fd.Close()
	if err != nil {
		l.Warnln("Saving .stignore:", err)
		return err
	}

	file := filepath.Join(cfg.Directory, ".stignore")
	err = osutil.Rename(fd.Name(), file)
	if err != nil {
		l.Warnln("Saving .stignore:", err)
		return err
	}

	return m.ScanRepo(repo)
}

// AddConnection adds a new peer connection to the model. An initial index will
// be sent to the connected peer, thereafter index updates whenever the local
// repository changes.
func (m *Model) AddConnection(rawConn io.Closer, protoConn protocol.Connection) {
	nodeID := protoConn.ID()

	m.pmut.Lock()
	if _, ok := m.protoConn[nodeID]; ok {
		panic("add existing node")
	}
	m.protoConn[nodeID] = protoConn
	if _, ok := m.rawConn[nodeID]; ok {
		panic("add existing node")
	}
	m.rawConn[nodeID] = rawConn

	cm := m.clusterConfig(nodeID)
	protoConn.ClusterConfig(cm)

	m.rmut.RLock()
	for _, repo := range m.nodeRepos[nodeID] {
		fs := m.repoFiles[repo]
		go sendIndexes(protoConn, repo, fs, m.repoIgnores[repo])
	}
	m.rmut.RUnlock()
	m.pmut.Unlock()

	m.nodeWasSeen(nodeID)
}

func (m *Model) nodeStatRef(nodeID protocol.NodeID) *stats.NodeStatisticsReference {
	m.rmut.Lock()
	defer m.rmut.Unlock()

	if sr, ok := m.nodeStatRefs[nodeID]; ok {
		return sr
	} else {
		sr = stats.NewNodeStatisticsReference(m.db, nodeID)
		m.nodeStatRefs[nodeID] = sr
		return sr
	}
}

func (m *Model) nodeWasSeen(nodeID protocol.NodeID) {
	m.nodeStatRef(nodeID).WasSeen()
}

func sendIndexes(conn protocol.Connection, repo string, fs *files.Set, ignores ignore.Patterns) {
	nodeID := conn.ID()
	name := conn.Name()
	var err error

	if debug {
		l.Debugf("sendIndexes for %s-%s/%q starting", nodeID, name, repo)
	}

	minLocalVer, err := sendIndexTo(true, 0, conn, repo, fs, ignores)

	for err == nil {
		time.Sleep(5 * time.Second)
		if fs.LocalVersion(protocol.LocalNodeID) <= minLocalVer {
			continue
		}

		minLocalVer, err = sendIndexTo(false, minLocalVer, conn, repo, fs, ignores)
	}

	if debug {
		l.Debugf("sendIndexes for %s-%s/%q exiting: %v", nodeID, name, repo, err)
	}
}

func sendIndexTo(initial bool, minLocalVer uint64, conn protocol.Connection, repo string, fs *files.Set, ignores ignore.Patterns) (uint64, error) {
	nodeID := conn.ID()
	name := conn.Name()
	batch := make([]protocol.FileInfo, 0, indexBatchSize)
	currentBatchSize := 0
	maxLocalVer := uint64(0)
	var err error

	fs.WithHave(protocol.LocalNodeID, func(fi protocol.FileIntf) bool {
		f := fi.(protocol.FileInfo)
		if f.LocalVersion <= minLocalVer {
			return true
		}

		if f.LocalVersion > maxLocalVer {
			maxLocalVer = f.LocalVersion
		}

		if ignores.Match(f.Name) {
			return true
		}

		if len(batch) == indexBatchSize || currentBatchSize > indexTargetSize {
			if initial {
				if err = conn.Index(repo, batch); err != nil {
					return false
				}
				if debug {
					l.Debugf("sendIndexes for %s-%s/%q: %d files (<%d bytes) (initial index)", nodeID, name, repo, len(batch), currentBatchSize)
				}
				initial = false
			} else {
				if err = conn.IndexUpdate(repo, batch); err != nil {
					return false
				}
				if debug {
					l.Debugf("sendIndexes for %s-%s/%q: %d files (<%d bytes) (batched update)", nodeID, name, repo, len(batch), currentBatchSize)
				}
			}

			batch = make([]protocol.FileInfo, 0, indexBatchSize)
			currentBatchSize = 0
		}

		batch = append(batch, f)
		currentBatchSize += indexPerFileSize + len(f.Blocks)*IndexPerBlockSize
		return true
	})

	if initial && err == nil {
		err = conn.Index(repo, batch)
		if debug && err == nil {
			l.Debugf("sendIndexes for %s-%s/%q: %d files (small initial index)", nodeID, name, repo, len(batch))
		}
	} else if len(batch) > 0 && err == nil {
		err = conn.IndexUpdate(repo, batch)
		if debug && err == nil {
			l.Debugf("sendIndexes for %s-%s/%q: %d files (last batch)", nodeID, name, repo, len(batch))
		}
	}

	return maxLocalVer, err
}

func (m *Model) updateLocal(repo string, f protocol.FileInfo) {
	f.LocalVersion = 0
	m.rmut.RLock()
	m.repoFiles[repo].Update(protocol.LocalNodeID, []protocol.FileInfo{f})
	m.rmut.RUnlock()
	events.Default.Log(events.LocalIndexUpdated, map[string]interface{}{
		"repo":     repo,
		"name":     f.Name,
		"modified": time.Unix(f.Modified, 0),
		"flags":    fmt.Sprintf("0%o", f.Flags),
		"size":     f.Size(),
	})
}

func (m *Model) requestGlobal(nodeID protocol.NodeID, repo, name string, offset int64, size int, hash []byte) ([]byte, error) {
	m.pmut.RLock()
	nc, ok := m.protoConn[nodeID]
	m.pmut.RUnlock()

	if !ok {
		return nil, fmt.Errorf("requestGlobal: no such node: %s", nodeID)
	}

	if debug {
		l.Debugf("%v REQ(out): %s: %q / %q o=%d s=%d h=%x", m, nodeID, repo, name, offset, size, hash)
	}

	return nc.Request(repo, name, offset, size)
}

func (m *Model) AddRepo(cfg config.RepositoryConfiguration) {
	if m.started {
		panic("cannot add repo to started model")
	}
	if len(cfg.ID) == 0 {
		panic("cannot add empty repo id")
	}

	m.rmut.Lock()
	m.repoCfgs[cfg.ID] = cfg
	m.repoFiles[cfg.ID] = files.NewSet(cfg.ID, m.db)

	m.repoNodes[cfg.ID] = make([]protocol.NodeID, len(cfg.Nodes))
	for i, node := range cfg.Nodes {
		m.repoNodes[cfg.ID][i] = node.NodeID
		m.nodeRepos[node.NodeID] = append(m.nodeRepos[node.NodeID], cfg.ID)
	}

	m.addedRepo = true
	m.rmut.Unlock()
}

func (m *Model) ScanRepos() {
	m.rmut.RLock()
	var repos = make([]string, 0, len(m.repoCfgs))
	for repo := range m.repoCfgs {
		repos = append(repos, repo)
	}
	m.rmut.RUnlock()

	var wg sync.WaitGroup
	wg.Add(len(repos))
	for _, repo := range repos {
		repo := repo
		go func() {
			err := m.ScanRepo(repo)
			if err != nil {
				invalidateRepo(m.cfg, repo, err)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func (m *Model) CleanRepos() {
	m.rmut.RLock()
	var dirs = make([]string, 0, len(m.repoCfgs))
	for _, cfg := range m.repoCfgs {
		dirs = append(dirs, cfg.Directory)
	}
	m.rmut.RUnlock()

	var wg sync.WaitGroup
	wg.Add(len(dirs))
	for _, dir := range dirs {
		w := &scanner.Walker{
			Dir:       dir,
			TempNamer: defTempNamer,
		}
		go func() {
			w.CleanTempFiles()
			wg.Done()
		}()
	}
	wg.Wait()
}

func (m *Model) ScanRepo(repo string) error {
	return m.ScanRepoSub(repo, "")
}

func (m *Model) ScanRepoSub(repo, sub string) error {
	if p := filepath.Clean(filepath.Join(repo, sub)); !strings.HasPrefix(p, repo) {
		return errors.New("invalid subpath")
	}

	m.rmut.RLock()
	fs, ok := m.repoFiles[repo]
	dir := m.repoCfgs[repo].Directory

	ignores, _ := ignore.Load(filepath.Join(dir, ".stignore"))
	m.repoIgnores[repo] = ignores

	w := &scanner.Walker{
		Dir:          dir,
		Sub:          sub,
		Ignores:      ignores,
		BlockSize:    scanner.StandardBlockSize,
		TempNamer:    defTempNamer,
		CurrentFiler: cFiler{m, repo},
		IgnorePerms:  m.repoCfgs[repo].IgnorePerms,
	}
	m.rmut.RUnlock()
	if !ok {
		return errors.New("no such repo")
	}

	m.setState(repo, RepoScanning)
	fchan, err := w.Walk()

	if err != nil {
		return err
	}
	batchSize := 100
	batch := make([]protocol.FileInfo, 0, 00)
	for f := range fchan {
		events.Default.Log(events.LocalIndexUpdated, map[string]interface{}{
			"repo":     repo,
			"name":     f.Name,
			"modified": time.Unix(f.Modified, 0),
			"flags":    fmt.Sprintf("0%o", f.Flags),
			"size":     f.Size(),
		})
		if len(batch) == batchSize {
			fs.Update(protocol.LocalNodeID, batch)
			batch = batch[:0]
		}
		batch = append(batch, f)
	}
	if len(batch) > 0 {
		fs.Update(protocol.LocalNodeID, batch)
	}

	batch = batch[:0]
	// TODO: We should limit the Have scanning to start at sub
	seenPrefix := false
	fs.WithHaveTruncated(protocol.LocalNodeID, func(fi protocol.FileIntf) bool {
		f := fi.(protocol.FileInfoTruncated)
		if !strings.HasPrefix(f.Name, sub) {
			// Return true so that we keep iterating, until we get to the part
			// of the tree we are interested in. Then return false so we stop
			// iterating when we've passed the end of the subtree.
			return !seenPrefix
		}

		seenPrefix = true
		if !protocol.IsDeleted(f.Flags) {
			if f.IsInvalid() {
				return true
			}

			if len(batch) == batchSize {
				fs.Update(protocol.LocalNodeID, batch)
				batch = batch[:0]
			}

			if ignores.Match(f.Name) {
				// File has been ignored. Set invalid bit.
				nf := protocol.FileInfo{
					Name:     f.Name,
					Flags:    f.Flags | protocol.FlagInvalid,
					Modified: f.Modified,
					Version:  f.Version, // The file is still the same, so don't bump version
				}
				events.Default.Log(events.LocalIndexUpdated, map[string]interface{}{
					"repo":     repo,
					"name":     f.Name,
					"modified": time.Unix(f.Modified, 0),
					"flags":    fmt.Sprintf("0%o", f.Flags),
					"size":     f.Size(),
				})
				batch = append(batch, nf)
			} else if _, err := os.Stat(filepath.Join(dir, f.Name)); err != nil && os.IsNotExist(err) {
				// File has been deleted
				nf := protocol.FileInfo{
					Name:     f.Name,
					Flags:    f.Flags | protocol.FlagDeleted,
					Modified: f.Modified,
					Version:  lamport.Default.Tick(f.Version),
				}
				events.Default.Log(events.LocalIndexUpdated, map[string]interface{}{
					"repo":     repo,
					"name":     f.Name,
					"modified": time.Unix(f.Modified, 0),
					"flags":    fmt.Sprintf("0%o", f.Flags),
					"size":     f.Size(),
				})
				batch = append(batch, nf)
			}
		}
		return true
	})
	if len(batch) > 0 {
		fs.Update(protocol.LocalNodeID, batch)
	}

	m.setState(repo, RepoIdle)
	return nil
}

// clusterConfig returns a ClusterConfigMessage that is correct for the given peer node
func (m *Model) clusterConfig(node protocol.NodeID) protocol.ClusterConfigMessage {
	cm := protocol.ClusterConfigMessage{
		ClientName:    m.clientName,
		ClientVersion: m.clientVersion,
		Options: []protocol.Option{
			{
				Key:   "name",
				Value: m.nodeName,
			},
		},
	}

	m.rmut.RLock()
	for _, repo := range m.nodeRepos[node] {
		cr := protocol.Repository{
			ID: repo,
		}
		for _, node := range m.repoNodes[repo] {
			// NodeID is a value type, but with an underlying array. Copy it
			// so we don't grab aliases to the same array later on in node[:]
			node := node
			// TODO: Set read only bit when relevant
			cn := protocol.Node{
				ID:    node[:],
				Flags: protocol.FlagShareTrusted,
			}
			if nodeCfg := m.cfg.GetNodeConfiguration(node); nodeCfg.Introducer {
				cn.Flags |= protocol.FlagIntroducer
			}
			cr.Nodes = append(cr.Nodes, cn)
		}
		cm.Repositories = append(cm.Repositories, cr)
	}
	m.rmut.RUnlock()

	return cm
}

func (m *Model) setState(repo string, state repoState) {
	m.smut.Lock()
	oldState := m.repoState[repo]
	changed, ok := m.repoStateChanged[repo]
	if state != oldState {
		m.repoState[repo] = state
		m.repoStateChanged[repo] = time.Now()
		eventData := map[string]interface{}{
			"repo": repo,
			"to":   state.String(),
		}
		if ok {
			eventData["duration"] = time.Since(changed).Seconds()
			eventData["from"] = oldState.String()
		}
		events.Default.Log(events.StateChanged, eventData)
	}
	m.smut.Unlock()
}

func (m *Model) State(repo string) (string, time.Time) {
	m.smut.RLock()
	state := m.repoState[repo]
	changed := m.repoStateChanged[repo]
	m.smut.RUnlock()
	return state.String(), changed
}

func (m *Model) Override(repo string) {
	m.rmut.RLock()
	fs := m.repoFiles[repo]
	m.rmut.RUnlock()

	m.setState(repo, RepoScanning)
	batch := make([]protocol.FileInfo, 0, indexBatchSize)
	fs.WithNeed(protocol.LocalNodeID, func(fi protocol.FileIntf) bool {
		need := fi.(protocol.FileInfo)
		if len(batch) == indexBatchSize {
			fs.Update(protocol.LocalNodeID, batch)
			batch = batch[:0]
		}

		have := fs.Get(protocol.LocalNodeID, need.Name)
		if have.Name != need.Name {
			// We are missing the file
			need.Flags |= protocol.FlagDeleted
			need.Blocks = nil
		} else {
			// We have the file, replace with our version
			need = have
		}
		need.Version = lamport.Default.Tick(need.Version)
		need.LocalVersion = 0
		batch = append(batch, need)
		return true
	})
	if len(batch) > 0 {
		fs.Update(protocol.LocalNodeID, batch)
	}
	m.setState(repo, RepoIdle)
}

// CurrentLocalVersion returns the change version for the given repository.
// This is guaranteed to increment if the contents of the local repository has
// changed.
func (m *Model) CurrentLocalVersion(repo string) uint64 {
	m.rmut.Lock()
	defer m.rmut.Unlock()

	fs, ok := m.repoFiles[repo]
	if !ok {
		panic("bug: LocalVersion called for nonexistent repo " + repo)
	}

	return fs.LocalVersion(protocol.LocalNodeID)
}

// RemoteLocalVersion returns the change version for the given repository, as
// sent by remote peers. This is guaranteed to increment if the contents of
// the remote or global repository has changed.
func (m *Model) RemoteLocalVersion(repo string) uint64 {
	m.rmut.Lock()
	defer m.rmut.Unlock()

	fs, ok := m.repoFiles[repo]
	if !ok {
		panic("bug: LocalVersion called for nonexistent repo " + repo)
	}

	var ver uint64
	for _, n := range m.repoNodes[repo] {
		ver += fs.LocalVersion(n)
	}

	return ver
}

func (m *Model) availability(repo string, file string) []protocol.NodeID {
	m.rmut.Lock()
	defer m.rmut.Unlock()

	fs, ok := m.repoFiles[repo]
	if !ok {
		return nil
	}

	return fs.Availability(file)
}

func (m *Model) String() string {
	return fmt.Sprintf("model@%p", m)
}