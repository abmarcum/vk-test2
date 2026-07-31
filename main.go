// registering /healthz directly rather than through this handler).
func HTTPSRedirectHandler(httpsPort int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}

		target := url.URL{
			Scheme:   "https",
			Host:     net.JoinHostPort(host, strconv.Itoa(httpsPort)),
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}

		if httpsPort == 443 {
			target.Host = host
		}

		w.Header().Set("Connection", "close")
		http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
	}
}

// LoggingRecoverMiddleware wraps an http.Handler with panic recovery so
// that a single misbehaving handler cannot crash the whole server process;
// panics are logged with a stack-free, redacted message and converted to a
// 500 response, matching the error-masking security mandate.
func LoggingRecoverMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic_recovered",
					"path", redactPath(r.URL.Path),
					"method", r.Method,
				)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal server error"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
