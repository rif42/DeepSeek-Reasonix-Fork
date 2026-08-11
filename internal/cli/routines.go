package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"reasonix/internal/bot"
	"reasonix/internal/botruntime"
	"reasonix/internal/config"
	"reasonix/internal/routines"

	"github.com/spf13/pflag"
)

// routinesHomeChatEnv returns the env var name that overrides a platform's
// home chat id for routine delivery (bare "platform" / "all" targets).
func routinesHomeChatEnv(platform string) string {
	return "REASONIX_ROUTINES_" + strings.ToUpper(platform) + "_HOME_CHAT"
}

func routinesCommand(args []string, version string) int {
	if len(args) < 1 {
		routinesUsage()
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "start":
		return routinesStart(rest, version)
	case "cron":
		return routinesCron(rest)
	case "webhook":
		return routinesWebhook(rest)
	case "help", "--help", "-h":
		routinesUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown routines subcommand %q\n\n", sub)
		routinesUsage()
		return 2
	}
}

func routinesUsage() {
	fmt.Fprint(os.Stderr, `reasonix routines — scheduled jobs and webhook automations

Usage:
  reasonix routines start [--dir DIR] [--model MODEL] [--webhook-addr ADDR]   run the scheduler + webhook receiver
  reasonix routines cron create <schedule> <prompt> [--name N] [--model M]    create a scheduled job
                        [--deliver T] [--script PATH] [--no-agent] [--repeat N]
  reasonix routines cron list                                                  list jobs
  reasonix routines cron show <id>                                             show one job
  reasonix routines cron pause|resume <id>                                     pause / resume a job
  reasonix routines cron remove <id>                                           delete a job
  reasonix routines cron run <id>                                              dispatch one job now
  reasonix routines cron tick                                                  dispatch every due job now
  reasonix routines webhook subscribe <slug> --prompt P [--events E] [--secret S]  subscribe a webhook route
                        [--deliver T] [--deliver-only]
  reasonix routines webhook list                                               list subscriptions
  reasonix routines webhook remove <slug>                                      delete a subscription
  reasonix routines webhook test <slug>                                        print the endpoint URL

Schedules: "30m" | "2h" | "every 30m" | "0 2 * * *" (5-field cron) | RFC3339 timestamp
Deliver targets: local | origin | all | platform | platform:chat[:thread]
`)
}

// routinesOpenStore opens the routines store under <reasonix-home>/routines.
func routinesOpenStore() (*routines.Store, error) {
	dir := routines.RoutinesDataDir()
	if dir == "" {
		return nil, fmt.Errorf("cannot resolve the reasonix home directory (set REASONIX_HOME)")
	}
	return routines.NewStore(dir)
}

func routinesCron(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines cron <create|list|show|pause|resume|remove|run|tick>")
		return 2
	}
	store, err := routinesOpenStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	switch args[0] {
	case "create":
		return routinesCronCreate(store, args[1:])
	case "list":
		return routinesCronList(store, args[1:])
	case "show":
		return routinesCronShow(store, args[1:])
	case "pause":
		return routinesCronSetPaused(store, args[1:], true)
	case "resume":
		return routinesCronSetPaused(store, args[1:], false)
	case "remove":
		return routinesCronRemove(store, args[1:])
	case "run":
		return routinesCronRun(store, args[1:])
	case "tick":
		return routinesCronTick(store, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown cron subcommand %q\n", args[0])
		return 2
	}
}

func routinesCronCreate(store *routines.Store, args []string) int {
	fs := pflag.NewFlagSet("routines cron create", pflag.ContinueOnError)
	fs.SetInterspersed(true)
	name := fs.String("name", "", "job name")
	model := fs.String("model", "", "model (empty = routines.model / default_model)")
	deliver := fs.String("deliver", "local", "delivery target: local|origin|all|platform[:chat[:thread]]")
	script := fs.String("script", "", "pre-run script path (stdout becomes prompt context)")
	noAgent := fs.Bool("no-agent", false, "script IS the job; stdout delivered verbatim")
	repeat := fs.Int("repeat", 0, "max runs (0 = unlimited)")
	workdir := fs.String("workdir", "", "script working directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines cron create <schedule> <prompt> [flags]")
		return 2
	}
	scheduleInput := fs.Arg(0)
	prompt := strings.TrimSpace(fs.Arg(1))
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "error: prompt is empty")
		return 1
	}
	now := time.Now()
	sched, err := routines.ParseSchedule(scheduleInput, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	next := sched.NextRun(now)
	if next == nil {
		fmt.Fprintln(os.Stderr, "error: schedule has no future run time")
		return 1
	}
	job := &routines.Job{
		ID:        newRoutinesID(),
		Name:      strings.TrimSpace(*name),
		Prompt:    prompt,
		Model:     strings.TrimSpace(*model),
		Script:    strings.TrimSpace(*script),
		NoAgent:   *noAgent,
		Schedule:  sched,
		Repeat:    routines.Repeat{Times: *repeat},
		Enabled:   true,
		State:     routines.JobStateIdle,
		Deliver:   strings.TrimSpace(*deliver),
		Workdir:   strings.TrimSpace(*workdir),
		NextRunAt: next,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
	if job.Name == "" {
		job.Name = job.ID
	}
	if err := store.PutJob(job); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("created job %s (%s)\n", job.ID, job.Name)
	fmt.Printf("  schedule: %s\n  next run: %s\n", scheduleInput, next.UTC().Format(time.RFC3339))
	return 0
}

func routinesCronList(store *routines.Store, args []string) int {
	jobs, err := store.ListJobs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if len(jobs) == 0 {
		fmt.Println("no jobs")
		return 0
	}
	fmt.Printf("%-20s %-24s %-18s %-10s %-10s %s\n", "ID", "NAME", "NEXT RUN", "STATE", "STATUS", "DELIVER")
	for _, j := range jobs {
		next := "-"
		if j.NextRunAt != nil {
			next = j.NextRunAt.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-20s %-24s %-18s %-10s %-10s %s\n", j.ID, truncateString(j.Name, 24), next, j.State, j.LastStatus, j.Deliver)
	}
	return 0
}

func routinesCronShow(store *routines.Store, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines cron show <id>")
		return 2
	}
	job, err := store.GetJob(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if job == nil {
		fmt.Fprintf(os.Stderr, "error: no job %q\n", args[0])
		return 1
	}
	fmt.Printf("id:            %s\n", job.ID)
	fmt.Printf("name:          %s\n", job.Name)
	fmt.Printf("prompt:        %s\n", job.Prompt)
	fmt.Printf("schedule:      %s\n", job.Schedule.Display)
	fmt.Printf("model:         %s\n", job.Model)
	fmt.Printf("script:        %s\n", job.Script)
	fmt.Printf("no_agent:      %v\n", job.NoAgent)
	fmt.Printf("deliver:       %s\n", job.Deliver)
	fmt.Printf("repeat:        %d/%d\n", job.Repeat.Completed, job.Repeat.Times)
	fmt.Printf("enabled:       %v\n", job.Enabled)
	fmt.Printf("state:         %s\n", job.State)
	next := "-"
	if job.NextRunAt != nil {
		next = job.NextRunAt.UTC().Format(time.RFC3339)
	}
	fmt.Printf("next run:      %s\n", next)
	last := "-"
	if job.LastRunAt != nil {
		last = job.LastRunAt.UTC().Format(time.RFC3339)
	}
	fmt.Printf("last run:      %s\n", last)
	fmt.Printf("last status:   %s\n", job.LastStatus)
	if job.LastError != "" {
		fmt.Printf("last error:    %s\n", job.LastError)
	}
	if job.LastDeliveryError != "" {
		fmt.Printf("delivery error: %s\n", job.LastDeliveryError)
	}
	return 0
}

func routinesCronSetPaused(store *routines.Store, args []string, paused bool) int {
	if len(args) < 1 {
		verb := "resume"
		if paused {
			verb = "pause"
		}
		fmt.Fprintf(os.Stderr, "usage: reasonix routines cron %s <id>\n", verb)
		return 2
	}
	now := time.Now().UTC()
	_, err := store.UpdateJob(args[0], func(j *routines.Job) error {
		if paused {
			j.State = routines.JobStatePaused
			j.PausedAt = &now
			j.PausedReason = "cli"
		} else {
			j.State = routines.JobStateIdle
			j.PausedAt = nil
			j.PausedReason = ""
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("job %s %s\n", args[0], map[bool]string{true: "paused", false: "resumed"}[paused])
	return 0
}

func routinesCronRemove(store *routines.Store, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines cron remove <id>")
		return 2
	}
	ok, err := store.DeleteJob(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "error: no job %q\n", args[0])
		return 1
	}
	fmt.Printf("removed job %s\n", args[0])
	return 0
}

// routinesDispatch builds a one-shot scheduler with the real agent runner and
// fires one tick. Used by `cron run` / `cron tick`.
func routinesDispatch(store *routines.Store, workspaceRoot string) (*routines.Scheduler, error) {
	cfg, _ := config.Load()
	model := cfg.Routines.Model
	if model == "" {
		model = cfg.DefaultModel
	}
	runner := &routines.AgentRunner{
		WorkspaceRoot: workspaceRoot,
		SessionDir:    routines.RoutinesDataDir() + "/sessions",
	}
	s := routines.NewScheduler(routines.SchedulerOptions{
		Store:        store,
		Runner:       runner,
		DefaultModel: model,
	})
	return s, nil
}

func routinesCronRun(store *routines.Store, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines cron run <id>")
		return 2
	}
	job, err := store.GetJob(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if job == nil {
		fmt.Fprintf(os.Stderr, "error: no job %q\n", args[0])
		return 1
	}
	now := time.Now()
	if _, err := store.UpdateJob(job.ID, func(j *routines.Job) error {
		j.Enabled = true
		j.NextRunAt = &now
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	wd := job.Workdir
	if wd == "" {
		wd = "."
	}
	s, err := routinesDispatch(store, wd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	s.TickOnce(time.Now())
	s.Stop()
	fmt.Printf("dispatched job %s\n", job.ID)
	return 0
}

func routinesCronTick(store *routines.Store, args []string) int {
	wd := "."
	if len(args) > 0 {
		wd = args[0]
	}
	s, err := routinesDispatch(store, wd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	s.TickOnce(time.Now())
	s.Stop()
	fmt.Println("tick done")
	return 0
}

func routinesWebhook(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines webhook <subscribe|list|remove|test>")
		return 2
	}
	store, err := routinesOpenStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	switch args[0] {
	case "subscribe":
		return routinesWebhookSubscribe(store, args[1:])
	case "list":
		return routinesWebhookList(store, args[1:])
	case "remove":
		return routinesWebhookRemove(store, args[1:])
	case "test":
		return routinesWebhookTest(store, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown webhook subcommand %q\n", args[0])
		return 2
	}
}

func routinesWebhookSubscribe(store *routines.Store, args []string) int {
	fs := pflag.NewFlagSet("routines webhook subscribe", pflag.ContinueOnError)
	fs.SetInterspersed(true)
	prompt := fs.String("prompt", "", "prompt template; {event.field} interpolates the payload")
	events := fs.String("events", "", "comma-separated event names to accept (empty = all)")
	secret := fs.String("secret", "", "HMAC secret (auto-generated when empty)")
	deliver := fs.String("deliver", "local", "delivery target for results")
	deliverOnly := fs.Bool("deliver-only", false, "deliver the rendered payload without running the agent")
	desc := fs.String("description", "", "free-form description")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines webhook subscribe <slug> --prompt P [flags]")
		return 2
	}
	slug := strings.TrimSpace(fs.Arg(0))
	if slug == "" || strings.ContainsAny(slug, "/ \t") {
		fmt.Fprintln(os.Stderr, "error: invalid slug")
		return 1
	}
	*prompt = strings.TrimSpace(*prompt)
	if *prompt == "" && !*deliverOnly {
		fmt.Fprintln(os.Stderr, "error: --prompt is required (unless --deliver-only)")
		return 1
	}
	if *secret == "" {
		*secret = newRoutinesSecret()
	}
	now := time.Now().UTC()
	sub := &routines.WebhookSubscription{
		Slug:        slug,
		Description: strings.TrimSpace(*desc),
		Events:      splitCommaList(*events),
		Secret:      *secret,
		Prompt:      *prompt,
		Deliver:     strings.TrimSpace(*deliver),
		DeliverOnly: *deliverOnly,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := store.PutWebhook(sub); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("subscribed webhook %s\n", slug)
	fmt.Printf("  route:  POST /webhooks/%s\n", slug)
	fmt.Printf("  secret: %s\n", sub.Secret)
	return 0
}

func routinesWebhookList(store *routines.Store, args []string) int {
	subs, err := store.ListWebhooks()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if len(subs) == 0 {
		fmt.Println("no webhook subscriptions")
		return 0
	}
	fmt.Printf("%-24s %-24s %-12s %-8s %s\n", "SLUG", "EVENTS", "DELIVER", "ONLY", "SECRET")
	for _, s := range subs {
		events := strings.Join(s.Events, ",")
		if events == "" {
			events = "*"
		}
		sec := "no"
		if s.Secret != "" {
			sec = "yes"
		}
		fmt.Printf("%-24s %-24s %-12s %-8v %s\n", s.Slug, events, s.Deliver, s.DeliverOnly, sec)
	}
	return 0
}

func routinesWebhookRemove(store *routines.Store, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines webhook remove <slug>")
		return 2
	}
	ok, err := store.DeleteWebhook(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "error: no webhook %q\n", args[0])
		return 1
	}
	fmt.Printf("removed webhook %s\n", args[0])
	return 0
}

func routinesWebhookTest(store *routines.Store, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: reasonix routines webhook test <slug>")
		return 2
	}
	sub, err := store.GetWebhook(args[0])
	if err != nil || sub == nil {
		fmt.Fprintf(os.Stderr, "error: no webhook %q\n", args[0])
		return 1
	}
	addr := "127.0.0.1:8644"
	if cfg, _ := config.Load(); cfg.Routines.WebhookAddr != "" {
		addr = cfg.Routines.WebhookAddr
	}
	host := addr
	if h, _, err := splitHostPort(addr); err == nil && h != "" {
		host = addr
	}
	fmt.Printf("POST http://%s/webhooks/%s\n", host, sub.Slug)
	fmt.Printf("header: X-Hub-Signature-256: sha256=<hmac_sha256(secret, body)>\n")
	if sub.Secret != "" {
		fmt.Printf("secret: %s\n", sub.Secret)
	}
	return 0
}

// routinesStart runs the scheduler + webhook receiver until interrupted.
func routinesStart(args []string, version string) int {
	fs := flag.NewFlagSet("routines start", flag.ContinueOnError)
	dir := fs.String("dir", "", "workspace root")
	model := fs.String("model", "", "default model (empty = config)")
	webhookAddr := fs.String("webhook-addr", "", "webhook listen address (default 127.0.0.1:8644)")
	noWebhook := fs.Bool("no-webhook", false, "disable the webhook receiver")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}
	workspaceRoot := *dir
	if workspaceRoot == "" {
		if wd, err := os.Getwd(); err == nil {
			workspaceRoot = wd
		}
	}
	store, err := routinesOpenStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defaultModel := *model
	if defaultModel == "" {
		defaultModel = cfg.Routines.Model
	}
	if defaultModel == "" {
		defaultModel = cfg.DefaultModel
	}

	addr := *webhookAddr
	if addr == "" {
		addr = cfg.Routines.WebhookAddr
	}
	if addr == "" {
		addr = "127.0.0.1:8644"
	}
	maxParallel := cfg.Routines.MaxParallelJobs
	if maxParallel <= 0 {
		maxParallel = 4
	}
	rateLimit := cfg.Routines.WebhookRateLimit
	if rateLimit <= 0 {
		rateLimit = 30
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Delivery: local + any configured bot platform connections.
	router := &routines.DeliveryRouter{LocalDir: store.Dir(), Logger: logger.Info}
	router.Adapters = routinesPlatformAdapters(cfg, logger)

	runner := &routines.AgentRunner{
		WorkspaceRoot: workspaceRoot,
		SessionDir:    routines.RoutinesDataDir() + "/sessions",
	}
	sched := routines.NewScheduler(routines.SchedulerOptions{
		Store:         store,
		Runner:        runner,
		Deliverer:     router,
		DefaultModel:  defaultModel,
		MaxParallel:   maxParallel,
		OutputDir:     store.Dir(),
		WorkspaceRoot: workspaceRoot,
		Logger:        logger.Info,
	})
	sched.Start(ctx)
	defer sched.Stop()

	var webhookSrv *httpServerHandle
	if !*noWebhook {
		wh := routines.NewWebhookHandler(store)
		wh.Runner = runner
		wh.Deliverer = router
		wh.DefaultModel = defaultModel
		wh.RateLimit = rateLimit
		wh.Logger = logger.Info
		webhookSrv = startWebhookServer(ctx, addr, wh, logger)
		if webhookSrv.err != nil {
			fmt.Fprintf(os.Stderr, "error: start webhook receiver: %v\n", webhookSrv.err)
			return 1
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nshutting down...")
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "reasonix routines starting (model: %s)\n", defaultModel)
	fmt.Fprintf(os.Stderr, "scheduler: every 60s, max %d parallel\n", maxParallel)
	if webhookSrv != nil {
		fmt.Fprintf(os.Stderr, "webhooks:  http://%s/webhooks/{slug}\n", addr)
	}

	<-ctx.Done()
	return 0
}

// routinesPlatformAdapters wires delivery adapters from configured bot
// connections. Home chats come from REASONIX_ROUTINES_<PLATFORM>_HOME_CHAT.
func routinesPlatformAdapters(cfg *config.Config, logger *slog.Logger) map[string]routines.DeliveryAdapter {
	enabled, _ := botruntime.EnabledPlatforms(cfg, nil)
	if !botruntime.HasEnabledPlatform(enabled) {
		return nil
	}
	bindings := botruntime.AdapterBindings(cfg, enabled, botruntime.RequestedFeishuDomains(nil), logger)
	adapters := map[string]routines.DeliveryAdapter{}
	for _, b := range bindings {
		if b.Adapter == nil {
			continue
		}
		platform := string(b.Platform)
		adapters[platform] = routines.BotAdapterShim{
			Adapter:  b.Adapter,
			HomeChat: os.Getenv(routinesHomeChatEnv(platform)),
		}
	}
	return adapters
}

// --- helpers ---------------------------------------------------------------

func newRoutinesID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(b)
}

func newRoutinesSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "fallback-%d", time.Now().UnixNano()))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func splitCommaList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func splitHostPort(addr string) (string, string, error) {
	idx := strings.LastIndexByte(addr, ':')
	if idx < 0 {
		return "", "", fmt.Errorf("no port in %q", addr)
	}
	return addr[:idx], addr[idx+1:], nil
}

// httpServerHandle wraps an http.Server with graceful shutdown and a bound
// error captured synchronously (ListenAndServe fails async otherwise).
type httpServerHandle struct {
	srv *http.Server
	err error
}

func startWebhookServer(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) *httpServerHandle {
	srv := &http.Server{Addr: addr, Handler: handler}
	h := &httpServerHandle{srv: srv}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		h.err = err
		return h
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("webhook server", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	return h
}

// bot.Adapter binding guards: keep the import used even when no platform is
// wired (the type appears in BotAdapterShim).
var _ = bot.PlatformQQ
