// Command cc-agent lets Claude Code sessions of different people exchange
// messages through a SoT server.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/genail/cc-agent/internal/config"
	"github.com/genail/cc-agent/internal/control"
	"github.com/genail/cc-agent/internal/relay"
	"github.com/genail/cc-agent/internal/service"
	"github.com/genail/cc-agent/internal/sot"
)

// version is set by the release build.
var version = "dev"

const (
	sessionEnv      = "CLAUDE_CODE_SESSION_ID"
	stdinArg        = "-"
	shutdownTimeout = 3 * time.Second
	exitUsage       = 2
)

const usage = `cc-agent - messages between Claude Code sessions of different people

Setup
  cc-agent server add <name> --url <url>   add a SoT server; asks for your token (or reads it from stdin)
  cc-agent server list
  cc-agent server remove <name>
  cc-agent service install | uninstall     run the daemon as a system service
  cc-agent daemon                          run the daemon in this terminal
  cc-agent status

Sharing a session (nothing is shared by default)
  cc-agent register --to <user> [--to <user>...] | --everyone
  cc-agent unregister

Messages
  cc-agent list                            sessions you can reach
  cc-agent users                           users of the server
  cc-agent send --to-user <user> --to-session <name or id> <text | ->
  cc-agent send --reply-to <message id> <text | ->

  cc-agent version

Options
  --server <name>   needed only with several servers
  --session <id>    session to act as; default: the current one ($` + sessionEnv + `)
`

func main() {
	err := run(os.Args[1:])
	var usageErr usageError
	switch {
	case errors.As(err, &usageErr):
		fmt.Fprintf(os.Stderr, "cc-agent: %v\n\n%s", err, usage)
		os.Exit(exitUsage)
	case err != nil:
		fmt.Fprintln(os.Stderr, "cc-agent:", err)
		os.Exit(1)
	}
}

type usageError struct{ text string }

func (e usageError) Error() string { return e.text }

func run(args []string) error {
	if len(args) == 0 {
		return usageError{"missing command"}
	}
	dir, err := config.DefaultDir()
	if err != nil {
		return err
	}
	daemon := control.NewClient(dir)
	command, rest := args[0], args[1:]
	switch command {
	case "server":
		return runServer(dir, rest)
	case "service":
		return runService(dir, rest)
	case "daemon":
		return runDaemon(dir)
	case "status":
		return runStatus(daemon)
	case "register":
		return runRegister(daemon, rest)
	case "unregister":
		return runUnregister(daemon, rest)
	case "list":
		return runList(daemon, rest)
	case "users":
		return runUsers(daemon, rest)
	case "send":
		return runSend(daemon, rest)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	return usageError{fmt.Sprintf("unknown command %q", command)}
}

// options are the flags shared by several commands.
type options struct {
	set             *flag.FlagSet
	server, session *string
}

func newOptions(name string) options {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return options{set, set.String("server", "", ""), set.String("session", os.Getenv(sessionEnv), "")}
}

func (o options) parse(args []string) error {
	if err := o.set.Parse(args); err != nil {
		return usageError{err.Error()}
	}
	return nil
}

func (o options) parseOnly(args []string) error {
	if err := o.parse(args); err != nil {
		return err
	}
	if o.set.NArg() > 0 {
		return usageError{fmt.Sprintf("unexpected argument %q", o.set.Arg(0))}
	}
	return nil
}

func (o options) requireSession() error {
	if *o.session == "" {
		return errors.New("not inside a Claude Code session; pass --session <id>")
	}
	return nil
}

func runServer(dir config.Dir, args []string) error {
	if len(args) == 0 {
		return usageError{"server: missing subcommand"}
	}
	cfg, err := dir.LoadConfig()
	if err != nil {
		return err
	}
	switch {
	case args[0] == "list" && len(args) == 1:
		for _, s := range cfg.Servers {
			fmt.Printf("%s\t%s\n", s.Name, s.URL)
		}
		return nil
	case args[0] == "remove" && len(args) == 2:
		kept := slices.DeleteFunc(slices.Clone(cfg.Servers), func(s config.Server) bool { return s.Name == args[1] })
		if len(kept) == len(cfg.Servers) {
			return fmt.Errorf("unknown server %q", args[1])
		}
		return dir.SaveConfig(config.Config{Servers: kept})
	case args[0] == "add" && len(args) >= 2:
		set := flag.NewFlagSet("server add", flag.ContinueOnError)
		set.SetOutput(io.Discard)
		url := set.String("url", "", "")
		if err := set.Parse(args[2:]); err != nil || set.NArg() > 0 || *url == "" {
			return usageError{"server add: expected <name> --url <url>"}
		}
		return addServer(dir, cfg, args[1], *url)
	}
	return usageError{"server: expected add, list or remove"}
}

func addServer(dir config.Dir, cfg config.Config, name, url string) error {
	if slices.ContainsFunc(cfg.Servers, func(s config.Server) bool { return s.Name == name }) {
		return fmt.Errorf("server %q already exists; remove it first", name)
	}
	token, err := readToken()
	if err != nil {
		return err
	}
	server, err := config.NewServer(name, url, token)
	if err != nil {
		return err
	}
	user, err := sot.New(server.URL, server.Token).WhoAmI(context.Background())
	if err != nil {
		return err
	}
	cfg.Servers = append(cfg.Servers, server)
	if err := dir.SaveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Added %s (%s). You are %q there.\n", server.Name, server.URL, user)
	return nil
}

func readToken() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprint(os.Stderr, "SoT token: ")
	token, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(string(token)), err
}

func runService(dir config.Dir, args []string) error {
	if len(args) != 1 {
		return usageError{"service: expected install or uninstall"}
	}
	switch args[0] {
	case "install":
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		if binary, err = filepath.EvalSymlinks(binary); err != nil {
			return err
		}
		if err := dir.Ensure(); err != nil {
			return err
		}
		if err := service.Install(binary, dir.LogPath()); err != nil {
			return err
		}
		fmt.Println("Service installed and started.")
		return nil
	case "uninstall":
		return service.Uninstall()
	}
	return usageError{"service: expected install or uninstall"}
}

func runDaemon(dir config.Dir) error {
	machine, err := os.Hostname()
	if err != nil {
		return err
	}
	connect := func(s config.Server) relay.API { return sot.New(s.URL, s.Token) }
	r, err := relay.New(dir, relay.ClaudeCode{}, connect, machine, log.Printf)
	if err != nil {
		return err
	}
	listener, err := control.Listen(dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{Handler: control.Handler(dir, r, version), ReadHeaderTimeout: shutdownTimeout}
	go r.Run(ctx)
	go func() {
		<-ctx.Done()
		timeout, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(timeout)
	}()
	log.Printf("cc-agent %s listening on %s", version, dir.SocketPath())
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runStatus(daemon *control.Client) error {
	var status control.Status
	if err := daemon.Call("/status", struct{}{}, &status); err != nil {
		return err
	}
	fmt.Printf("Daemon %s is running.\n", status.Version)
	for _, s := range status.Servers {
		state := "ok"
		if s.Error != "" {
			state = s.Error
		}
		fmt.Printf("Server %s (%s): %s\n", s.Name, s.URL, state)
	}
	for _, r := range status.Registrations {
		fmt.Printf("Shared session %s on %s with %s\n", r.SessionID, r.Server, audience(r))
	}
	return nil
}

func audience(r config.Registration) string {
	if r.Everyone {
		return "everyone"
	}
	return strings.Join(r.VisibleTo, ", ")
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runRegister(daemon *control.Client, args []string) error {
	o := newOptions("register")
	var users stringList
	o.set.Var(&users, "to", "")
	everyone := o.set.Bool("everyone", false, "")
	if err := o.parseOnly(args); err != nil {
		return err
	}
	if *everyone == (len(users) > 0) {
		return usageError{"register: pass either --to <user> or --everyone"}
	}
	if err := o.requireSession(); err != nil {
		return err
	}
	var done config.Registration
	in := config.Registration{Server: *o.server, SessionID: *o.session, Everyone: *everyone, VisibleTo: users}
	if err := daemon.Call("/register", in, &done); err != nil {
		return err
	}
	fmt.Printf("Session %s is shared on %s with %s.\n", done.SessionID, done.Server, audience(done))
	return nil
}

func runUnregister(daemon *control.Client, args []string) error {
	o := newOptions("unregister")
	if err := o.parseOnly(args); err != nil {
		return err
	}
	if err := o.requireSession(); err != nil {
		return err
	}
	if err := daemon.Call("/unregister", config.SessionRef{Server: *o.server, SessionID: *o.session}, nil); err != nil {
		return err
	}
	fmt.Println("Session is no longer shared.")
	return nil
}

func runList(daemon *control.Client, args []string) error {
	o := newOptions("list")
	if err := o.parseOnly(args); err != nil {
		return err
	}
	var sessions []sot.Session
	if err := daemon.Call("/sessions", control.ServerRequest{Server: *o.server}, &sessions); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tSESSION\tMACHINE\tSTATE\tSESSION ID")
	for _, s := range sessions {
		owner, state := s.Owner, "offline"
		if s.Mine {
			owner += " (you)"
		}
		if s.Online {
			state = "online"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", owner, s.Name, s.Machine, state, s.SessionID)
	}
	return w.Flush()
}

func runUsers(daemon *control.Client, args []string) error {
	o := newOptions("users")
	if err := o.parseOnly(args); err != nil {
		return err
	}
	var users []string
	if err := daemon.Call("/users", control.ServerRequest{Server: *o.server}, &users); err != nil {
		return err
	}
	for _, u := range users {
		fmt.Println(u)
	}
	return nil
}

func runSend(daemon *control.Client, args []string) error {
	o := newOptions("send")
	toUser := o.set.String("to-user", "", "")
	toSession := o.set.String("to-session", "", "")
	replyTo := o.set.String("reply-to", "", "")
	if err := o.parse(args); err != nil {
		return err
	}
	direct := *toUser != "" && *toSession != "" && *replyTo == ""
	reply := *replyTo != "" && *toUser == "" && *toSession == ""
	if !direct && !reply {
		return usageError{"send: pass --to-user and --to-session, or --reply-to"}
	}
	if err := o.requireSession(); err != nil {
		return err
	}
	body := strings.Join(o.set.Args(), " ")
	if body == stdinArg {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body = string(data)
	}
	if strings.TrimSpace(body) == "" {
		return usageError{"send: the message is empty"}
	}
	var sent sot.Sent
	err := daemon.Call("/send", control.SendRequest{
		Server: *o.server, SessionID: *o.session,
		ToUser: *toUser, ToSession: *toSession, ReplyTo: *replyTo, Body: body,
	}, &sent)
	if err != nil {
		return err
	}
	fmt.Printf("Sent, id %s. Replies will appear in this session.\n", sent.ID)
	if direct && !sent.Online {
		fmt.Println("The recipient's session is offline; the message waits for it.")
	}
	return nil
}
