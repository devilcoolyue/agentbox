package server

func (s *Server) termUsageLoop() { s.usageService().Loop() }
func (s *Server) termWatchLoop() { s.usageService().Watch() }
