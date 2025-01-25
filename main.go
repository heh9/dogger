package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/buildkite/terminal-to-html/v3"
	"github.com/buraksezer/olric"
	"github.com/buraksezer/olric/config"
	"github.com/bwmarrin/discordgo"
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
		},
		Type: discordgo.ChatApplicationCommand,
	})
	if err != nil {
		slog.Error("Failed to create command", slog.Any("error", err))
		os.Exit(1)
	}

	dgo.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		// Don't process the interaction if the node is not the leader
		if !isCoordinatorNode(ctx, e) {
			return
		}

		key := uuid.New().String()
		f := i.ApplicationCommandData().Options[0].StringValue()
		content := fmt.Sprintf("You called %s, visit the logs at: http://127.0.0.1:8080/logs/%s", f, key)

		// Checkout the code from the source

		go func() {
			cmd := exec.Command("dagger", "call", f, "--progress=plain", "--source=.")

			out, err := cmd.CombinedOutput()
			if err != nil {
				slog.Error("Failed to execute dagger function", slog.Any("error", err))
			}

			if err := store.Put(ctx, key, string(out)); err != nil {
				slog.Error("Failed to put value", slog.Any("error", err))
				return
			}

			t, err := time.ParseDuration(*expiration)
			if err != nil {
				slog.Error("Failed to parse expiration", slog.Any("error", err))
				t = time.Duration(5 * time.Minute)
			}

			if err := store.Expire(ctx, key, t); err != nil {
				slog.Error("Failed to expire key", slog.Any("error", err))
			}
		}()

		dgo.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content,
			},
		})

		slog.Info("Interaction received", slog.Any("content", content))
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
