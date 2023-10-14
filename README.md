test-stream-http-duplex
-----------------------

Demo of full duplex streaming of json over HTTP using Go. From the naive
approach, it is necessary to enable full duplex communication, flush the http
status code OK to the client, then flush all new messages to the client.

To run the demo, run `go run ./ --log-level=DEBUG` where each ping/pong request
response will be logged.

The key diff to enable such streaming is the following diff.

```diff
diff --git a/main.go b/main.go
index 5775f58..393b592 100644
--- a/main.go
+++ b/main.go
@@ -99,11 +99,27 @@ func client(ctx context.Context, address string) error {
 func server(ctx context.Context, address string) error {
 	mux := http.NewServeMux()
 	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
+		respCtl := http.NewResponseController(writer)
+		err := respCtl.EnableFullDuplex()
+		if err != nil {
+			slog.Warn("failed to enable full duplex on http writer", "error", err)
+			return
+		}
+
 		var inMsg requestMsg
 		outMsg := responseMsg{Msg: "pong"}
 		dec := json.NewDecoder(request.Body)
 		enc := json.NewEncoder(writer)
 
+		// to get the communication going
+		writer.WriteHeader(http.StatusOK)
+		err = respCtl.Flush()
+		if err != nil {
+			slog.Error("server: failed to flush status header to client", "error", err)
+			return
+		}
+		slog.Info("server: wrote status ok to client")
+
 		for {
 			select {
 			case <-request.Context().Done():
@@ -130,6 +146,11 @@ func server(ctx context.Context, address string) error {
 					slog.Info("server: client closed connection - finished")
 					return
 				}
+				err = respCtl.Flush()
+				if err != nil {
+					slog.Error("server: failed to flush request message to client", "error", err)
+					return
+				}
 				slog.Info("server: sent pong to client")
 			}
 		}
```
