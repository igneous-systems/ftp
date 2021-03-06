package ftp

import (
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const mdtmFormat = "20060102150405"

var errNoDataConn = errors.New("no data channel connection")

// A Handler for a session.
type Handler interface {
	// Handle a session. It is optional to send a greeting or reply to a QUIT.
	// The session is closed by the Server on return. The return value is for
	// composing Handlers and is ignored by the Server.
	Handle(*Session) error
}

var _ Handler = (*FileHandler)(nil)

// An Authorizer can be used with a FileHandler to handle login.
type Authorizer interface {
	// Authorize the user. Returning an error closes the session.
	Authorize(user, pass string) (bool, error)
}

// A FileHandler serves from a FileSystem.
type FileHandler struct {
	Authorizer // Authorizer for login. If nil, accept all.
	FileSystem // FileSystem to serve.
}

// Handle implements Handler.
func (h *FileHandler) Handle(s *Session) error {
	fs := fileSession{
		FileHandler: h,
		Session:     s,
	}
	return fs.Handle()
}

// A fileSession wraps session state for a FileHandler.
type fileSession struct {
	*FileHandler
	*Session

	authed   bool   // Whether we're done with auth.
	renaming string // The file we're renaming, if any.
	epsvOnly bool   // Whether we saw "EPSV ALL".
	restart  int64  // Restart offset.
}

func (s *fileSession) Handle() error {
	for {
		c, err := s.Command()
		if err != nil {
			return err
		}
		if err := s.handle(c); err != nil {
			return err
		}
		if c.Cmd == "QUIT" {
			return io.EOF
		}
		if c.Cmd != "RNFR" {
			s.renaming = ""
		}
		if c.Cmd != "REST" {
			s.restart = 0
		}
	}
}

func (s *fileSession) handle(c *Command) error {
	if !s.authed {
		return s.handlePreAuth(c)
	}
	return s.handlePostAuth(c)
}

func (s *fileSession) handlePreAuth(c *Command) error {
	switch c.Cmd {
	case "USER":
		if s.authed {
			return s.Reply(530, "Cannot change user.")
		}
		if c.Msg == "" {
			return s.Reply(504, "A user name is required.")
		}
		s.User = c.Msg
		return s.Reply(331, "Please specify the password.")
	case "PASS":
		if s.authed {
			return s.Reply(230, "Already logged in.")
		}
		if s.User == "" {
			return s.Reply(503, "Log in with USER first.")
		}
		if s.Authorizer != nil {
			if ok, err := s.Authorize(s.User, c.Msg); err != nil {
				s.User = ""
				return err
			} else if !ok {
				s.User = ""
				return s.Reply(430, "Invalid user name or password.")
			}
		}
		s.Password = c.Msg
		s.authed = true
		return s.Reply(230, "Login successful.")
	case "FEAT":
		msg := []string{"Extensions supported:"}
		msg = append(msg, s.features()...)
		msg = append(msg, "End.")
		return s.Reply(211, strings.Join(msg, "\n"))
	case "QUIT":
		return s.Reply(211, "Goodbye.")
	default:
		if !s.authed {
			return s.Reply(530, "Log in with USER and PASS.")
		}
		return s.Reply(502, "Not implemented.")
	}
}

func (s *fileSession) handlePostAuth(c *Command) error {
	switch c.Cmd {
	case "SYST":
		return s.Reply(215, "UNIX Type: L8")
	case "TYPE":
		if err := s.SetType(c.Msg); err != nil {
			return s.Reply(504, err.Error())
		}
		return s.Reply(200, "Type switched successfully.")
	case "MODE":
		if err := s.SetMode(c.Msg); err != nil {
			return s.Reply(504, err.Error())
		}
		return s.Reply(200, "Mode switched successfully.")
	case "PWD":
		path := s.Path("")
		return s.Reply(257, "%s is the current directory.", quote(path))
	case "CWD":
		if c.Msg == "" {
			return s.Reply(550, "Failed to change directory.")
		}
		path := s.Path(c.Msg)
		if stat, err := s.Stat(path); isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such directory.")
		} else if err != nil || !stat.IsDir() {
			return s.Reply(550, "Failed to change directory.")
		}
		s.Dir = path
		return s.Reply(250, "Directory successfully changed.")
	case "CDUP":
		path := s.Path("..")
		if stat, err := s.Stat(path); isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such directory.")
		} else if err != nil || !stat.IsDir() {
			return s.Reply(550, "Failed to change directory.")
		}
		s.Dir = path
		return s.Reply(250, "Directory successfully changed.")
	case "MKD":
		path := s.Path(c.Msg)
		if err := s.Mkdir(path); err != nil {
			return s.Reply(550, "Failed to create directory.")
		}
		return s.Reply(257, "%s created.", quote(path))
	case "SIZE":
		path := s.Path(c.Msg)
		stat, err := s.Stat(path)
		if isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such file.")
		} else if err != nil {
			return s.Reply(550, "Could not get size.")
		} else if stat.IsDir() {
			return s.Reply(550, "Path specifies a directory.")
		}
		size := strconv.FormatInt(stat.Size(), 10)
		return s.Reply(213, size)
	case "MDTM":
		path := s.Path(c.Msg)
		stat, err := s.Stat(path)
		if isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such file or directory.")
		} else if err != nil || stat.IsDir() {
			return s.Reply(550, "Could not get size.")
		}
		mdtm := stat.ModTime().Format(mdtmFormat)
		return s.Reply(213, mdtm)
	case "DELE", "RMD":
		if c.Msg == "" {
			return s.Reply(501, "A file name is required.")
		}
		path := s.Path(c.Msg)
		if err := s.Remove(path); isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such file.")
		} else if err != nil {
			return s.Reply(550, "Could not delete file.")
		}
		return s.Reply(250, "Successfully deleted file.")
	case "RNFR":
		if c.Msg == "" {
			return s.Reply(501, "A file name is required.")
		}
		s.renaming = s.Path(c.Msg)
		return s.Reply(350, "Call RNTO to specify destination.")
	case "RNTO":
		if c.Msg == "" {
			return s.Reply(501, "A file name is required.")
		} else if s.renaming == "" {
			return s.Reply(503, "Call RNFR first.")
		}
		old, new := s.renaming, s.Path(c.Msg)
		if err := s.Rename(old, new); isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such file.")
		} else if err != nil {
			return s.Reply(550, "Could not rename file.")
		}
		return s.Reply(250, "Successfully renamed file.")
	case "PASV":
		if s.epsvOnly {
			return s.Reply(550, "PASV is disallowed.")
		}
		if err := s.Passive("tcp4"); err != nil {
			println(err.Error())
			return s.Reply(425, "Can't open data connection.")
		}
		hp := s.Data.HostPort()
		return s.Reply(227, "Entering Passive Mode (%s).", hp)
	case "EPSV":
		if msg := strings.ToUpper(c.Msg); msg == "ALL" {
			s.epsvOnly = true
			return s.Reply(200, "EPSV ALL ok.")
		}
		var nw string
		switch c.Msg {
		case "1":
			nw = "tcp4"
		case "2":
			nw = "tcp6"
		case "":
			nw = s.Addr.Network()
		default:
			return s.Reply(522, "Unsupported protocol.")
		}
		if err := s.Passive(nw); err != nil {
			return s.Reply(425, "Can't open data connection.")
		}
		p := s.Data.Port()
		return s.Reply(229, "Entering Extended Passive Mode (|||%d|)", p)
	case "PORT":
		if s.epsvOnly {
			return s.Reply(550, "PORT is disallowed.")
		}
		addr, err := ParsePORT(c.Msg)
		if err != nil {
			return s.Reply(501, "Invalid syntax.")
		}
		if err := s.Active(addr); err != nil {
			return s.Reply(550, "Failed to connect.")
		}
		return s.Reply(200, "OK")
	case "EPRT":
		if s.epsvOnly {
			return s.Reply(550, "EPRT is disallowed.")
		}
		addr, err := ParseEPRT(c.Msg)
		if err != nil {
			return s.Reply(501, "Invalid syntax.")
		}
		if err := s.Active(addr); err != nil {
			return s.Reply(550, "Failed to connect.")
		}
		return s.Reply(200, "OK")
	case "REST":
		n, err := strconv.ParseInt(c.Msg, 10, 64)
		if err != nil || n < 0 {
			return s.Reply(501, "Invalid syntax.")
		}
		s.restart = n
		return s.Reply(350, "Restart position accepted (%d).", n)
	case "STAT":
		if c.Msg == "" {
			return s.Reply(211, "Looks good to me.")
		}
		list, err := s.stat(c.Msg)
		if isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such file or directory.")
		} else if err != nil {
			return s.Reply(550, "Error retrieving status.")
		}
		msg := []string{"Status:"}
		msg = append(msg, listLines(list)...)
		msg = append(msg, "End.")
		return s.Reply(213, strings.Join(msg, "\n"))
	case "LIST", "NLST":
		if err := s.list(c); err == errNoDataConn {
			return s.Reply(425, "Use PORT or PASV first.")
		} else if isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such directory.")
		} else if err != nil {
			return s.Reply(550, "Error listing directory.")
		}
		return s.Reply(226, "Directory send OK.")
	case "RETR":
		if err := s.retrieve(c); err == errNoDataConn {
			return s.Reply(425, "Use PORT or PASV first.")
		} else if isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if isNotExist(err) {
			return s.Reply(550, "No such file.")
		} else if err != nil {
			return s.Reply(550, "Error retrieving file.")
		}
		return s.Reply(226, "Transfer complete.")
	case "STOR":
		if err := s.store(c); err == errNoDataConn {
			return s.Reply(425, "Use PORT or PASV first.")
		} else if isPermission(err) {
			return s.Reply(550, "Insufficient permissions.")
		} else if err != nil {
			return s.Reply(550, "Error storing file.")
		}
		return s.Reply(226, "Transfer complete.")
	case "PBSZ":
		if s.Server.TLS == nil {
			return s.Reply(502, "Not implemented.")
		}
		if c.Msg == "0" {
			return s.Reply(200, "OK.")
		}
		return s.Reply(534, "Unacceptable buffer size. PBSZ=0")
	case "PROT":
		if s.Server.TLS == nil {
			return s.Reply(502, "Not implemented.")
		}
		switch c.Msg {
		case "P":
			s.TLS = s.Server.TLS
		case "C":
			s.TLS = nil
		default:
			return s.Reply(504, "Unsupported protection level.")
		}
		return s.Reply(200, "Protection level changed.")
	case "OPTS":
		if msg := strings.ToUpper(c.Msg); msg == "UTF8 ON" {
			return s.Reply(200, "Always in UTF8 mode.")
		}
		return s.Reply(501, "Option not understood.")
	case "HELP":
		return s.Reply(214,
			`The following commands are recognized.
CDUP CWD  DELE EPRT EPSV FEAT HELP LIST MDTM MKD  MODE NLST NOOP OPTS
PASS PASV PBSZ PORT PROT PWD  QUIT REST RETR RMD  RNFR RNTO SIZE STAT
STOR SYST TYPE USER
Help OK.`)
	case "NOOP":
		return s.Reply(200, "OK.")
	default:
		return s.handlePreAuth(c)
	}
}

// Return supported features.
func (s *fileSession) features() []string {
	f := []string{
		"EPRT", "EPSV", "MDTM", "PASV", "REST STREAM", "SIZE", "UTF8",
	}
	if s.Server.TLS != nil {
		f = append(f, "PBSZ", "PROT")
	}
	sort.Strings(f)
	return f
}

// Handler for RETR.
func (s *fileSession) retrieve(c *Command) error {
	if s.Data == nil {
		return errNoDataConn
	}
	path := s.Path(c.Msg)
	file, err := s.Open(path)
	if err != nil {
		s.CloseData()
		return err
	}
	if err := s.Reply(150, "Here comes the file."); err != nil {
		file.Close()
		s.CloseData()
		return err
	}
	if s.restart > 0 {
		if _, err := file.Seek(s.restart, io.SeekStart); err != nil {
			file.Close()
			s.CloseData()
			return err
		}
	}
	if _, err := io.Copy(s.Data, file); err != nil {
		file.Close()
		s.CloseData()
		return err
	}
	file.Close()
	return s.CloseData()
}

// Handler for STOR.
func (s *fileSession) store(c *Command) error {
	if s.Data == nil {
		return errNoDataConn
	}
	path := s.Path(c.Msg)
	file, err := s.Create(path)
	if err != nil {
		s.CloseData()
		return err
	}
	if err := s.Reply(150, "Awaiting file data."); err != nil {
		file.Close()
		s.CloseData()
		return err
	}
	if s.restart > 0 {
		if _, err := file.Seek(s.restart, io.SeekStart); err != nil {
			file.Close()
			s.CloseData()
			return err
		}
	}
	if _, err := io.Copy(file, s.Data); err != nil {
		file.Close()
		s.CloseData()
		return err
	}
	err = file.Close()
	s.CloseData()
	return err
}

// Handler for STAT.
func (s *fileSession) stat(p string) ([]os.FileInfo, error) {
	stat, err := s.Stat(p)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		return []os.FileInfo{stat}, nil
	}
	file, err := s.Open(p)
	if err != nil {
		return nil, err
	}
	list, err := file.Readdir(0)
	if err != nil {
		file.Close()
		return nil, err
	}
	file.Close()
	return list, nil
}

// Handler for LIST and NLST.
func (s *fileSession) list(c *Command) error {
	if s.Data == nil {
		return errNoDataConn
	}
	path := s.Path(stripListFlags(c.Msg))
	file, err := s.Open(path)
	if err != nil {
		s.CloseData()
		return err
	}
	if err := s.Reply(150, "Here comes the list."); err != nil {
		file.Close()
		s.CloseData()
		return err
	}
	list := Lister{
		File: file,
		Cmd:  c.Cmd,
	}
	if _, err := list.WriteTo(s.Data); err != nil {
		file.Close()
		s.CloseData()
		return err
	}
	file.Close()
	return s.CloseData()
}

// Some clients assume LIST accepts flags like ls. This removes those.
func stripListFlags(s string) string {
	for _, c := range s {
		if c == '-' {
			break
		} else if c != ' ' {
			return s
		}
	}
	ss := strings.Split(s, " ")
	out := ss[:0]
	for _, s := range ss {
		if !strings.HasPrefix(s, "-") {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

// Check if an error is a permission error.
func isPermission(err error) bool {
	return os.IsPermission(err)
}

// Check if an error implies a file does not exist.
func isNotExist(err error) bool {
	return os.IsNotExist(err)
}

// Check if an error implies a file already exists.
func isExist(err error) bool {
	return os.IsPermission(err)
}

// Quote returns a quoted string with double-escaped quotes.
func quote(s string) string {
	return `"` + strings.Replace(s, `"`, `""`, -1) + `"`
}
