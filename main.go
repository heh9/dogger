package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/buildkite/terminal-to-html/v3"
	"github.com/buraksezer/olric"
	"github.com/buraksezer/olric/config"
	"github.com/bwmarrin/discordgo"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/lmittmann/tint"
	slogmulti "github.com/samber/slog-multi"
)

var (
	token      = flag.String("token", "", "Bot Token")
	expiration = flag.String("expiration", "", "Expiration time in seconds")
	level      = flag.String("level", "info", "Log level")
	selector   = flag.String("selector", "", "Kubernetes selector")
	guild      = flag.String("guild", "", "Discord guild")
	fqdn       = flag.String("fqdn", "http://127.0.0.1:8080", "FQDN")
	repo       = flag.String("repo", "git@github.com:heh9/dogger.git", "Repository")
)

type slogWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func NewSlogWriter(logger *slog.Logger, level slog.Level) *slogWriter {
	return &slogWriter{
		logger: logger,
		level:  level,
	}
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	var level slog.Level

	msg := string(p)
	start := strings.Index(msg, "[")
	end := strings.Index(msg, "]")

	if start != -1 && end != -1 {
		if err := level.UnmarshalText([]byte(msg[start+1 : end])); err != nil {
			level = slog.LevelError
		}

		msg = msg[end+1:]
	}

	if len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}
	w.logger.Log(context.Background(), level, msg)

	return len(p), nil
}

func isCoordinatorNode(ctx context.Context, e *olric.EmbeddedClient) bool {
	if err := e.RefreshMetadata(ctx); err != nil {
		slog.Error("Failed to refresh metadata", slog.Any("error", err))
		return false
	}

	members, err := e.Members(ctx)
	if err != nil {
		slog.Error("Failed to get members", slog.Any("error", err))
		return false
	}
	for _, m := range members {
		if m.Coordinator {
			return true
		}
	}

	return false
}

type job struct {
	mu sync.Mutex

	canceled bool
	command  *exec.Cmd
}

func main() {
	w := os.Stderr

	flag.Parse()

	var logl slog.Level
	if err := logl.UnmarshalText([]byte(*level)); err != nil {
		slog.Error("Failed to parse log level", slog.Any("error", err))
		os.Exit(1)
	}

	opts := slog.HandlerOptions{
		AddSource: true,
		Level:     logl,
	}
	logger := slog.New(slogmulti.Fanout(
		tint.NewHandler(w, &tint.Options{
			Level:     opts.Level,
			AddSource: opts.AddSource,
			// Make sure to replace the error attribute with a tint error
			ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
				if a.Value.Kind() == slog.KindString && a.Value.String() == "" || a.Value.Any() == nil {
					return slog.Attr{}
				}

				if err, ok := a.Value.Any().(error); ok {
					aErr := tint.Err(err)
					aErr.Key = a.Key
					return aErr
				}

				return a
			},
		}),
	))
	slog.SetDefault(logger)

	logExpiration, err := time.ParseDuration(*expiration)
	if err != nil {
		slog.Error("Failed to parse expiration", slog.Any("error", err))
		logExpiration = time.Duration(60 * time.Minute)
	}

	ctx := context.Background()

	conf := config.New("local")
	conf.LogOutput = NewSlogWriter(logger, logl)
	octx, cancel := context.WithCancel(ctx)
	conf.Started = func() {
		defer cancel()
		slog.Info("Olric node is ready")
	}

	db, err := olric.New(conf)
	if err != nil {
		slog.Error("Failed to create olric instance", slog.Any("error", err))
		os.Exit(1)
	}

	go func() {
		// Call Start at background. It's a blocker call.
		if err = db.Start(); err != nil {
			slog.Error("olric.Start returned an error", slog.Any("error", err))
			os.Exit(1)
		}
	}()
	<-octx.Done()

	e := db.NewEmbeddedClient()
	store, err := e.NewDMap("dagger-jobs-map")
	if err != nil {
		slog.Error("Failed to create dmap", slog.Any("error", err))
		os.Exit(1)
	}
	slog.Info("DMap is ready", slog.Any("name", store.Name()))

	dgo, err := discordgo.New("Bot " + *token)
	if err != nil {
		slog.Error("Failed to create discord session", slog.Any("error", err))
		os.Exit(1)
	}
	defer dgo.Close()

	dgo.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		slog.Info("Bot is ready")
	})
	if err := dgo.Open(); err != nil {
		slog.Error("Failed to open discord session", slog.Any("error", err))
		os.Exit(1)
	}

	_, err = dgo.ApplicationCommandCreate(dgo.State.User.ID, *guild, &discordgo.ApplicationCommand{
		Name:        "call",
		Description: "Call a dagger function",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "function",
				Type:        discordgo.ApplicationCommandOptionString,
				Description: "The function to call",
				Required:    true,
			},
			{
				Name:        "source",
				Type:        discordgo.ApplicationCommandOptionString,
				Description: "The source directory to use",
				Required:    true,
			},
			{
				Name: "from",
				// Optional, uses master by default
				Type:        discordgo.ApplicationCommandOptionString,
				Description: "The commit hash or branch name to call on",
				Required:    false,
			},
		},
		Type: discordgo.ChatApplicationCommand,
	})
	if err != nil {
		slog.Error("Failed to create command", slog.Any("error", err))
		os.Exit(1)
	}

	jobs := make(map[string]*job)

	dgo.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		var (
			hasError bool
			content  string
		)

		start := time.Now()

		// Don't process the interaction if the node is not the leader
		if !isCoordinatorNode(ctx, e) || i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		key := uuid.New().String()

		args := make(map[string]string)
		for _, o := range i.ApplicationCommandData().Options {
			args[o.Name] = o.StringValue()
		}
		args["job id"] = key

		logOutput := ""
		logUri := fmt.Sprintf("%s/logs/%s", *fqdn, key)
		wg := sync.WaitGroup{}
		saveLog := func() {
			if err := store.Put(ctx, key, logOutput); err != nil {
				slog.Error("Failed to put value", slog.Any("error", err))
				return
			}

			if err := store.Expire(ctx, key, logExpiration); err != nil {
				slog.Error("Failed to expire key", slog.Any("error", err))
			}
		}

		workDir, err := os.MkdirTemp("", "dagger-job-")
		if err != nil {
			hasError = true
			slog.Error("Failed to create temporary directory", slog.Any("error", err))
			logOutput += fmt.Sprintf("Failed to create temporary directory: %v\n", err)
			return
		}

		embeds := NewEmbedBuilder().AddFields(args)
		messageComponents := NewMessageComponentsBuilder().AddLogsButton(logUri)

		finish := func() {
			wg.Wait()

			if err := os.RemoveAll(workDir); err != nil {
				slog.Error("Failed to remove temporary directory", slog.Any("error", err))
				logOutput += fmt.Sprintf("Failed to remove temporary directory: %v", err)
			}
			args["duration"] = time.Since(start).String()

			var status int

			content = "**Beautiful. Stunning. Perfection.**"
			if hasError {
				status = JobFailed
				content = "**Oh, come on! This is raw! Absolutely embarrassing.**"
			}
			if j, ok := jobs[key]; ok && j.canceled {
				status = JobCanceled
				content = "**What is this? A half-baked disaster? Canceled. Get it together!**"
			}

			embeds = embeds.AddFields(args)
			embeds = embeds.ApplyStatus(status)

			messageComponents = NewMessageComponentsBuilder().AddLogsButton(logUri).AddStatusButton(status)
			components := messageComponents.Build()

			_, err = dgo.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content:    &content,
				Components: &components,
				Embeds:     &[]*discordgo.MessageEmbed{embeds.Build()},
			})
			if err != nil {
				slog.Error("Failed to send followup message", slog.Any("error", err))
			}

			saveLog()
			delete(jobs, key)
		}
		defer finish()

		if args["from"] == "" {
			args["from"] = "master"
		}
		embeds = embeds.AddFields(args)

		var prog bytes.Buffer
		gitOpts := &git.CloneOptions{
			URL:           *repo,
			ReferenceName: plumbing.NewBranchReferenceName(args["from"]),
			Progress:      &prog,
		}

		_, err = git.PlainClone(workDir, false, gitOpts)
		logOutput += prog.String()
		if err != nil {
			hasError = true
			slog.Error("Failed to clone repository", slog.Any("error", err))
			logOutput += fmt.Sprintf("Failed to clone repository: %v\n", err)
			return
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			cmd := exec.Command(
				"dagger", "call", args["function"], "--progress=plain", fmt.Sprintf("--source=%s", args["source"]),
			)
			cmd.Dir = workDir
			jobs[key] = &job{command: cmd}

			out, err := cmd.CombinedOutput()
			if err != nil {
				hasError = true
				slog.Error("Failed to execute dagger function", slog.Any("error", err))
				logOutput += fmt.Sprintf("Failed to execute dagger function: %v\n", err)
			}

			logOutput += string(out)
		}()

		embeds = embeds.ApplyStatus(JobRunning)
		messageComponents = messageComponents.AddActionButton("Cancel job", key, true)

		err = dgo.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Data: &discordgo.InteractionResponseData{
				Components: messageComponents.Build(),
				Embeds:     []*discordgo.MessageEmbed{embeds.Build()},
				Content:    "**Move it, move it! This job won't cook itself!**",
			},
			Type: discordgo.InteractionResponseChannelMessageWithSource,
		})
		if err != nil {
			slog.Error("Failed to respond to interaction", slog.Any("error", err))
			return
		}

		slog.Info("Interaction received", slog.Any("content", content))
	})

	dgo.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		var (
			key     string
			content string
		)

		if !isCoordinatorNode(ctx, e) || i.Type != discordgo.InteractionMessageComponent {
			return
		}

		finish := func() {
			err := dgo.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Data: &discordgo.InteractionResponseData{
					Content: content,
				},
				Type: discordgo.InteractionResponseUpdateMessage,
			})
			if err != nil {
				slog.Error("Failed to send followup message", slog.Any("error", err))
			}
		}
		defer finish()

		key = i.MessageComponentData().CustomID

		j, ok := jobs[key]
		if !ok {
			content = fmt.Sprintf("Job `%s` not found\n\n", key)
			return
		}

		j.mu.Lock()
		defer j.mu.Unlock()

		if j.command != nil && j.command.Process != nil {
			// Use negative PID to kill the entire process group
			err := syscall.Kill(j.command.Process.Pid, syscall.SIGKILL)
			if err != nil {
				slog.Error("Failed to kill job process", slog.Any("error", err))
				content = fmt.Sprintf("Failed to kill job process: %v\n\n", err)
				return
			}

			j.canceled = true
		}

		content = fmt.Sprintf("Canceled job `%s`", key)
	})

	router := mux.NewRouter()

	sb, err := os.ReadFile("stylesheet.css")
	if err != nil {
		slog.Error("Failed to read terminal.css", slog.Any("error", err))
		os.Exit(1)
	}

	router.Handle(
		"/logs/{key}",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()

			key := mux.Vars(r)["key"]
			if key == "" {
				http.Error(w, "Key is required", http.StatusBadRequest)
				return
			}

			v, err := store.Get(ctx, key)
			if err != nil {
				http.Error(w, "Failed to get value", http.StatusInternalServerError)
				return
			}

			out, err := v.String()
			if err != nil {
				http.Error(w, "Failed to convert value to string", http.StatusInternalServerError)
				return
			}

			html := terminal.Render([]byte(out))

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(
				w,
				`<!DOCTYPE html>
				<html>
				<head>
					<style>%s</style>
				</head>
				<body style="background-color: #171717;">
					<div class="term-container">%s</div>
				</body>
				</html>`,
				sb,
				html,
			)
		}),
	)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: router,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				slog.Error("Failed to start app server", slog.Any("error", err))
				os.Exit(1)
			}
		}
	}()
	slog.Info("Server started", slog.String("address", srv.Addr))

	sg := make(chan os.Signal, 1)
	signal.Notify(sg, syscall.SIGINT, syscall.SIGTERM)
	<-sg

	if err := srv.Shutdown(context.Background()); err != nil {
		slog.ErrorContext(ctx, "Failed to shutdown app server", slog.Any("error", err))
	}
}
