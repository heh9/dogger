package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/buraksezer/olric"
	"github.com/buraksezer/olric/config"
	"github.com/bwmarrin/discordgo"
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
}

func (w *slogWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSuffix(string(p), "\n")

	switch {
	case strings.Contains(strings.ToLower(msg), "ERR"):
		w.logger.Error(msg)
	case strings.Contains(strings.ToLower(msg), "WARN"):
		w.logger.Warn(msg)
	default:
		w.logger.Info(msg)
	}

	return len(p), nil
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
	conf.Logger = log.New(&slogWriter{logger: logger}, "", 0)
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
		content := fmt.Sprintf("You called function %s", i.ApplicationCommandData().Options[0].StringValue())

		dgo.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content,
			},
		})

		slog.Info("Interaction received", slog.Any("content", content))
	})

	router := mux.NewRouter()

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
