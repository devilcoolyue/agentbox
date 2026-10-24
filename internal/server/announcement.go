package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"time"

	"agentbox/internal/announce"
	"agentbox/internal/buildinfo"
)

// 系统通告：管理员（或发布脚本经 `agentbox announce`）发一条全站横幅，告诉
// 所有用户即将发版或其他事项。内容存在 data/announcement.json；网页发布后
// 立即经 /api/usage/live 推一条 {"type":"announcement"}，命令行写入的由
// announcementLoop 轮询文件摘要发现。

// announcementPoll is how often the loop checks the file for outside writes.
const announcementPoll = 3 * time.Second

// activeWithin is how recently a workspace must have been used to appear in
// the administrator's "who would a restart affect" list.
const activeWithin = 30 * time.Minute

func (s *Server) announcements() *announce.Service {
	s.announceOnce.Do(func() { s.announce = announce.New(s.cfg.DataDir) })
	return s.announce
}

// announcementLoop completes a release this start fulfils, then pushes every
// change of the file to open pages.
func (s *Server) announcementLoop() {
	svc := s.announcements()
	if done, err := svc.Complete(buildinfo.Version, s.startedAt); err != nil {
		log.Printf("announcement: %v", err)
	} else if done {
		log.Printf("announcement: release completed as %s", buildinfo.Version)
	}
	last, _ := svc.Revision()
	for waitInterval(s.workContext(), announcementPoll) {
		rev, err := svc.Revision()
		if err != nil || rev == last {
			continue
		}
		last = rev
		s.usageLive.announce()
	}
}

type announcementView struct {
	Announcement *announce.Announcement `json:"announcement"`
	Now          int64                  `json:"now"` // server clock, for countdowns
}

func (s *Server) writeAnnouncement(w http.ResponseWriter, a *announce.Announcement) {
	now := time.Now()
	if !a.Visible(now) {
		a = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, announcementView{Announcement: a, Now: now.UnixMilli()})
}

func (s *Server) handleAnnouncement(w http.ResponseWriter, r *http.Request) {
	a, err := s.announcements().Get()
	if err != nil {
		// A broken file must not break every page; the administrator sees
		// the error in the log and can replace or clear it.
		log.Printf("announcement: %v", err)
	}
	s.writeAnnouncement(w, a)
}

func (s *Server) handleAnnouncementPut(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "通告内容过大")
		return
	}
	var in announce.Input
	if err = json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "通告格式无效")
		return
	}
	a, err := s.announcements().Put(in, reqUser(r).Name, buildinfo.Version, time.Now())
	var ve *announce.ValidationError
	if errors.As(err, &ve) {
		writeErr(w, http.StatusBadRequest, ve.Msg)
		return
	}
	if err != nil {
		log.Printf("announcement: save: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法保存系统通告")
		return
	}
	s.usageLive.announce()
	s.writeAnnouncement(w, a)
}

func (s *Server) handleAnnouncementDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.announcements().Clear(); err != nil {
		log.Printf("announcement: clear: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法撤下系统通告")
		return
	}
	s.usageLive.announce()
	s.writeAnnouncement(w, nil)
}

type onlineUser struct {
	User  string `json:"user"`
	Pages int    `json:"pages"`
}

type activeWorkspace struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	User        string `json:"user"`
	Agent       string `json:"agent"`
	Attached    int    `json:"attached"`     // open terminals and chat connections
	ActiveTurns int    `json:"active_turns"` // web chat turns a restart would cut off
	LastActive  int64  `json:"last_active"`  // ms; 0 when not seen since start
}

type announcementActivity struct {
	Online     []onlineUser      `json:"online"`
	Workspaces []activeWorkspace `json:"workspaces"`
	Now        int64             `json:"now"`
}

// handleAnnouncementActivity lists who a restart would affect right now:
// users with pages open, and workspaces that are attached, running a turn or
// used within activeWithin.
func (s *Server) handleAnnouncementActivity(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	out := announcementActivity{Online: []onlineUser{}, Workspaces: []activeWorkspace{}, Now: now.UnixMilli()}
	for user, pages := range s.usageLive.online() {
		out.Online = append(out.Online, onlineUser{User: user, Pages: pages})
	}
	sort.Slice(out.Online, func(i, j int) bool { return out.Online[i].User < out.Online[j].User })
	turns, err := s.store.ActiveChatTurns()
	if err != nil {
		log.Printf("announcement activity: %v", err)
		writeErr(w, http.StatusInternalServerError, "无法读取运行中的对话")
		return
	}
	seen := s.workspaces().Activity().Snapshot()
	for _, sess := range s.store.All() {
		state := seen[sess.ID]
		recent := !state.LastSeen.IsZero() && now.Sub(state.LastSeen) < activeWithin
		if state.Holds == 0 && turns[sess.ID] == 0 && !recent {
			continue
		}
		ws := activeWorkspace{ID: sess.ID, Name: sess.Name, User: sess.User, Agent: sess.Agent, Attached: state.Holds, ActiveTurns: turns[sess.ID]}
		if !state.LastSeen.IsZero() {
			ws.LastActive = state.LastSeen.UnixMilli()
		}
		out.Workspaces = append(out.Workspaces, ws)
	}
	sort.Slice(out.Workspaces, func(i, j int) bool {
		a, b := out.Workspaces[i], out.Workspaces[j]
		if (a.Attached+a.ActiveTurns > 0) != (b.Attached+b.ActiveTurns > 0) {
			return a.Attached+a.ActiveTurns > 0
		}
		return a.LastActive > b.LastActive
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
