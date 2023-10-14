package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sync/errgroup"
)

type requestMsg struct {
	Msg string
}
type responseMsg struct {
	Msg string
}

func client(ctx context.Context, address string) error {
	client := http.Client{
		Transport:     nil,
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	var resp *http.Response
	var w *io.PipeWriter
	for {
		// explicitly set r to be a io.Reader, as if not, NewRequestWithContext tries to close the reader before returning as PipeReader fulfills ReadeCloser interface
		var r *io.PipeReader
		r, w = io.Pipe()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, r)
		if err != nil {
			return fmt.Errorf("failed to create request, error was: %w", err)
		}

		select {
		case <-ctx.Done():
			slog.Info("client: context was done, exiting")
			return nil
		default:
			// fall-through
		}
		resp, err = client.Do(req)

		if err != nil {
			slog.Info("client: failed to start request against server", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			slog.Info("client: failed to start request against server", "statuscode", resp.StatusCode)
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
	defer resp.Body.Close()

	enc := json.NewEncoder(w)
	dec := json.NewDecoder(resp.Body)
	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ctx.Done():
			slog.Info("client: context was done, exiting")
			return nil
		case <-ticker.C:
			err := enc.Encode(requestMsg{
				Msg: "ping",
			})
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("client: failed to encode request message to server, error was: %w", err)
				}
				return nil
			}
			slog.Info("client: posted ping to server")
			var in responseMsg
			err = dec.Decode(&in)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("failed to decode response message from server, error was: %w", err)
				}
				return nil
			}
			slog.Info("client: received message from server", "msg", in.Msg)
		}
	}
}

func server(ctx context.Context, address string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		var inMsg requestMsg
		outMsg := responseMsg{Msg: "pong"}
		dec := json.NewDecoder(request.Body)
		enc := json.NewEncoder(writer)

		for {
			select {
			case <-request.Context().Done():
				return
			case <-ctx.Done():
				return
			default:
				err := dec.Decode(&inMsg)
				if err != nil {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						slog.Error("server: failed to receive request message from client", "error", err)
						return
					}
					slog.Info("server: client closed connection - finished")
					return
				}
				slog.Info("server: received message from client", "msg", inMsg.Msg)
				err = enc.Encode(outMsg)
				if err != nil {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						slog.Error("server: failed to send respond message to client", "error", err)
						return
					}
					slog.Info("server: client closed connection - finished")
					return
				}
				slog.Info("server: sent pong to client")
			}
		}
	})

	server := http.Server{
		Addr:                         address,
		Handler:                      mux,
		DisableGeneralOptionsHandler: false,
		TLSConfig:                    nil,
		ReadTimeout:                  0,
		ReadHeaderTimeout:            0,
		WriteTimeout:                 0,
		IdleTimeout:                  0,
		MaxHeaderBytes:               0,
		TLSNextProto:                 nil,
		ConnState:                    nil,
		ErrorLog:                     nil,
		BaseContext:                  nil,
		ConnContext:                  nil,
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		err := server.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	eg.Go(func() error {
		<-ctx.Done()
		slog.Info("server: context was done, shutting down server")
		timeoutCtx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFunc()
		defer slog.Info("server: finished shutting down")
		return server.Shutdown(timeoutCtx)
	})

	return eg.Wait()
}

func main() {
	ctx, cancelFunc := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancelFunc()
	address := "localhost:8080"
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { return server(ctx, address) })
	eg.Go(func() error { return client(ctx, "http://"+address) })
	eg.Go(func() error {
		<-ctx.Done()
		slog.Info("signal: interrupt signal received")
		return nil
	})

	err := eg.Wait()
	if err != nil {
		panic(err)
	}
}
