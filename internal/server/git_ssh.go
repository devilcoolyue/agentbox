package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentbox/internal/gitaccess"
	"agentbox/internal/store"
)

func gitRepositoryURL(raw string, c store.GitConnection) (string, error) {
	if c.AuthType == "ssh" {
		return gitaccess.SSHRepositoryURL(raw, c.BaseURL, c.Username)
	}
	return gitaccess.RepositoryURL(raw, c.BaseURL)
}
func (s *Server) openGitSSH(ctx context.Context, c store.GitConnection, repository string, write, v2 bool) (*gitaccess.SSHStream, error) {
	if err := s.checkGitTerminalScope(ctx, c, repository, write); err != nil {
		return nil, err
	}
	current, err := s.store.GitConnectionFor(gitActor(c), c.ID)
	if err != nil || !current.Enabled || current.Revision != c.Revision || write && current.ReadOnly {
		return nil, errors.New("SSH 连接已停用、修改或不允许写入")
	}
	raw, err := s.gitVault().Open(current.Secret, current.AssociatedData())
	if err != nil {
		return nil, err
	}
	credential, err := gitaccess.ParseSSHCredential(raw)
	if err != nil {
		return nil, err
	}
	return gitaccess.OpenSSH(ctx, repository, c.Username, credential, write, v2, func(ctx context.Context, network, address string) (net.Conn, error) {
		return s.gitDial(ctx, c, network, address)
	})
}
func (s *Server) gitSSHTransport(ctx context.Context, c store.GitConnection, repository string, write bool, ref, old, next string) (string, func(), error) {
	pb := s.cfg.GetProxyBridge()
	host, _, err := net.SplitHostPort(pb.Bind)
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return "", nil, errors.New("SSH Git 网桥不可用")
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		ln.Close()
		return "", nil, err
	}
	ticket := hex.EncodeToString(random)
	grantCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	go func() {
		defer close(done)
		defer workers.Wait()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			workers.Add(1)
			go func() {
				defer func() { <-slots }()
				defer workers.Done()
				defer conn.Close()
				stop := context.AfterFunc(grantCtx, func() { conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(120 * time.Second))
				version2, err := gitaccess.ReadGitRequest(conn, ticket, write)
				if err != nil {
					return
				}
				stream, err := s.openGitSSH(grantCtx, c, repository, write, version2)
				if err != nil {
					return
				}
				defer stream.Close()
				uploads := make(chan struct{})
				go func() {
					defer close(uploads)
					var dst io.Writer = stream.Writer
					if op := gitLive(ctx); op != nil {
						dst = gitCountWriter{Writer: dst, count: op.written.Add}
					}
					var copyErr error
					if write {
						_, copyErr = gitaccess.ForwardPush(dst, conn, old, next, ref)
					} else {
						_, copyErr = io.Copy(dst, conn)
					}
					_ = stream.Writer.Close()
					if copyErr != nil {
						stream.Close()
						conn.Close()
					}
				}()
				var dst io.Writer = conn
				if op := gitLive(ctx); op != nil {
					dst = gitCountWriter{Writer: conn, count: op.read.Add}
				}
				_, _ = io.Copy(dst, stream)
				_ = stream.Wait()
				conn.Close()
				<-uploads
			}()
		}
	}()
	stop := context.AfterFunc(grantCtx, func() { ln.Close() })
	closeFn := func() { cancel(); ln.Close(); stop(); <-done }
	return "git://" + net.JoinHostPort(pb.Host, fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)) + "/" + ticket, closeFn, nil
}

type gitCountWriter struct {
	io.Writer
	count func(int64) int64
}

func (w gitCountWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.count(int64(n))
	return n, err
}
func (s *Server) testGitSSH(w http.ResponseWriter, r *http.Request, c store.GitConnection, repository string) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	op, err := s.beginGitOperation(ctx, gitActor(c), "", "", c.ID, "connection.test", repository)
	if err != nil {
		writeGitStoreErr(w, err)
		return
	}
	result := "failed"
	defer func() { s.finishGitOperation(ctx, op, result) }()
	stream, err := s.openGitSSH(ctx, c, repository, false, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer stream.Close()
	// A valid advertisement is packet-framed and begins with an object ID (or
	// the empty repository's zero ID). Do not retain/log refs or protocol bytes.
	var header [4]byte
	if _, err = io.ReadFull(stream, header[:]); err != nil {
		writeErr(w, 502, "SSH 仓库未返回 Git 引用")
		return
	}
	n, err := parseGitPacketLength(header[:])
	if err != nil || n < 4 {
		writeErr(w, 502, "SSH Git 协议响应无效")
		return
	}
	data := make([]byte, n-4)
	if _, err = io.ReadFull(stream, data); err != nil {
		writeErr(w, 502, "SSH Git 协议读取失败")
		return
	}
	oid, _, hasRef := strings.Cut(string(data), " ")
	if !hasRef || !gitOID(oid) {
		writeErr(w, 502, "目标不是可读取的 SSH Git 仓库")
		return
	}
	result = "success"
	writeJSON(w, 200, map[string]any{"read_access": true, "write_access": "untested", "tested_at": time.Now().UTC().Format(time.RFC3339Nano)})
}

func parseGitPacketLength(header []byte) (int, error) {
	n, err := strconv.ParseUint(string(header), 16, 16)
	if err != nil || n > 65520 {
		return 0, errors.New("invalid Git packet")
	}
	return int(n), nil
}
