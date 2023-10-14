package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

type requestMsg struct {
	Msg string
}
type responseMsg struct {
	Msg string
}

const ContentTypeNdJson = "application/x-ndjson"

var clientPings atomic.Uint64
var clientWriteBytes atomic.Uint64

type countWriter struct {
	w     io.Writer
	count uint64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.count += uint64(n)
	return n, err
}

func client(ctx context.Context, address string) error {
	client := http.Client{
		Transport:     nil,
		CheckRedirect: nil,
		Jar:           nil,
		Timeout:       0,
	}

	var resp *http.Response
	var pw *io.PipeWriter
	for {
		// explicitly set r to be a io.Reader, as if not, NewRequestWithContext tries to close the reader before returning as PipeReader fulfills ReadeCloser interface
		var r *io.PipeReader
		r, pw = io.Pipe()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, r)
		if err != nil {
			return fmt.Errorf("failed to create request, error was: %w", err)
		}

		req.Header.Set("Accept", ContentTypeNdJson)
		req.Header.Set("Content-Type", ContentTypeNdJson)

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

	w := &countWriter{
		w: pw,
	}
	enc := json.NewEncoder(w)
	dec := json.NewDecoder(resp.Body)
	ticker := time.NewTicker(1 * time.Second)
	tChan := ticker.C
	if true {
		dummyChan := make(chan time.Time)
		close(dummyChan)
		tChan = dummyChan
	}
	for {
		select {
		case <-ctx.Done():
			slog.Info("client: context was done, exiting")
			return nil
		case <-tChan:
			err := enc.Encode(requestMsg{
				Msg: "ping",
			})
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("client: failed to encode request message to server, error was: %w", err)
				}
				return nil
			}
			_, err = io.WriteString(w, "\n")
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("client: failed to send newline to server, error was: %w", err)
				}
				return nil
			}
			slog.Debug("client: posted ping to server")
			clientPings.Add(1)
			clientWriteBytes.Store(w.count)
			continue
			var in responseMsg
			err = dec.Decode(&in)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return fmt.Errorf("failed to decode response message from server, error was: %w", err)
				}
				return nil
			}
			slog.Debug("client: received message from server", "msg", in.Msg)
			clientPings.Add(1)
			clientWriteBytes.Store(w.count)
		}
	}
}

var serverPongs atomic.Uint64

func server(ctx context.Context, hostPort string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if method := request.Method; method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writer.WriteHeader(http.StatusMethodNotAllowed)
			slog.Info("server: client attempted to connect with wrong method instead of POST", "wrong_method", method)
			return
		}

		if contentType := request.Header.Get("Content-Type"); contentType != ContentTypeNdJson {
			writer.WriteHeader(http.StatusUnsupportedMediaType)
			slog.Info("server: client attempted to connect with wrong content-type instead of "+ContentTypeNdJson, "wrong_content_type", contentType)
			return
		}

		if accepts := request.Header.Get("Accept"); accepts != ContentTypeNdJson {
			writer.WriteHeader(http.StatusNotAcceptable)
			slog.Info("server: client requested data in wrong format instead of "+ContentTypeNdJson, "wrong_accept", accepts)
			return
		}

		respCtl := http.NewResponseController(writer)
		err := respCtl.EnableFullDuplex()
		if err != nil {
			slog.Warn("failed to enable full duplex on http writer", "error", err)
			return
		}

		var inMsg requestMsg
		outMsg := responseMsg{Msg: "pong"}
		dec := json.NewDecoder(request.Body)
		enc := json.NewEncoder(writer)

		// to get the communication going
		writer.WriteHeader(http.StatusOK)
		err = respCtl.Flush()
		if err != nil {
			slog.Error("server: failed to flush status header to client", "error", err)
			return
		}
		slog.Info("server: wrote status ok to client")

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
				slog.Debug("server: received message from client", "msg", inMsg.Msg)
				continue
				err = enc.Encode(outMsg)
				if err != nil {
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						slog.Error("server: failed to send respond message to client", "error", err)
						return
					}
					slog.Info("server: client closed connection - finished")
					return
				}
				_, err = io.WriteString(writer, "\n")
				if err != nil {
					if !errors.Is(err, io.EOF) {
						slog.Error("server: failed to send newline to client", "error", err)
					}
					slog.Info("server: client closed connection - finished")
					return
				}
				err = respCtl.Flush()
				if err != nil {
					slog.Error("server: failed to flush request message to client", "error", err)
					return
				}
				slog.Debug("server: sent pong to client")
				serverPongs.Add(1)
			}
		}
	})

	server := http.Server{
		Addr:                         hostPort,
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
	var level slog.Level = slog.LevelInfo
	hostPort := "localhost:8080"
	flag.TextVar(&level, "log-level", level, "set log level")
	flag.StringVar(&hostPort, "hostport", hostPort, "set hostPort")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		AddSource:   false,
		Level:       level,
		ReplaceAttr: nil,
	})))

	ctx, cancelFunc := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancelFunc()
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { return server(ctx, hostPort) })
	eg.Go(func() error { return client(ctx, "http://"+hostPort) })
	eg.Go(func() error {
		<-ctx.Done()
		slog.Info("signal: interrupt signal received")
		return nil
	})
	eg.Go(func() error {
		ticker := time.NewTicker(5 * time.Second)
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				now := time.Now()
				s := now.Sub(start).Seconds()
				slog.Info("measurements",
					"serverPongsPerSecond", float64(serverPongs.Load())/s,
					"clientPingsPerSecond", float64(clientPings.Load())/s,
					"clientWriteMBytesPerSecond", float64(clientWriteBytes.Load())/s/(1024*1024),
				)
			}
		}
		return nil
	})

	err := eg.Wait()
	if err != nil {
		panic(err)
	}
}
