package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func newTestRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("erro ao subir miniredis: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	cleanup := func() {
		_ = rdb.Close()
		mr.Close()
	}

	return rdb, cleanup
}

func newJSONServer(t *testing.T, status int, body interface{}) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "" {
			t.Fatalf("header Authorization não enviado")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

func TestHealthHandler(t *testing.T) {
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	app.healthHandler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status esperado %d, obtido %d", http.StatusOK, resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("erro ao decodificar resposta: %v", err)
	}

	if body["status"] != "ok" {
		t.Fatalf("status esperado 'ok', obtido '%s'", body["status"])
	}
}

func TestEvaluationHandler_MissingParams(t *testing.T) {
	app := &App{}
	req := httptest.NewRequest(http.MethodGet, "/evaluate", nil)
	w := httptest.NewRecorder()

	app.evaluationHandler(w, req)

	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status esperado %d, obtido %d", http.StatusBadRequest, w.Result().StatusCode)
	}
}

func TestEvaluationHandler_NotFoundReturnsFalse(t *testing.T) {
	rdb, cleanup := newTestRedis(t)
	defer cleanup()

	flagSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer flagSrv.Close()

	targetingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer targetingSrv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		RedisClient:         rdb,
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL:      flagSrv.URL,
		TargetingServiceURL: targetingSrv.URL,
	}

	req := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=u1&flag_name=flag-x", nil)
	w := httptest.NewRecorder()

	app.evaluationHandler(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status esperado %d, obtido %d", http.StatusOK, w.Result().StatusCode)
	}

	var body EvaluationResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&body); err != nil {
		t.Fatalf("erro ao decodificar resposta: %v", err)
	}

	if body.Result != false {
		t.Fatalf("resultado esperado false, obtido %v", body.Result)
	}
}

func TestEvaluationHandler_GenericErrorReturnsBadGateway(t *testing.T) {
	rdb, cleanup := newTestRedis(t)
	defer cleanup()

	flagSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "erro interno", http.StatusInternalServerError)
	}))
	defer flagSrv.Close()

	targetingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer targetingSrv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		RedisClient:         rdb,
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL:      flagSrv.URL,
		TargetingServiceURL: targetingSrv.URL,
	}

	req := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=u1&flag_name=flag-x", nil)
	w := httptest.NewRecorder()

	app.evaluationHandler(w, req)

	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status esperado %d, obtido %d", http.StatusBadGateway, w.Result().StatusCode)
	}
}

func TestEvaluationHandler_Success(t *testing.T) {
	rdb, cleanup := newTestRedis(t)
	defer cleanup()

	flagSrv := newJSONServer(t, http.StatusOK, Flag{
		ID:        1,
		Name:      "enable-dashboard",
		IsEnabled: true,
	})
	defer flagSrv.Close()

	targetingSrv := newJSONServer(t, http.StatusOK, TargetingRule{
		ID:        1,
		FlagName:  "enable-dashboard",
		IsEnabled: true,
		Rules: Rule{
			Type:  "PERCENTAGE",
			Value: 100.0,
		},
	})
	defer targetingSrv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		RedisClient:         rdb,
		SqsSvc:              nil, // evita envio real
		SqsQueueURL:         "",
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL:      flagSrv.URL,
		TargetingServiceURL: targetingSrv.URL,
	}

	req := httptest.NewRequest(http.MethodGet, "/evaluate?user_id=u1&flag_name=enable-dashboard", nil)
	w := httptest.NewRecorder()

	app.evaluationHandler(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status esperado %d, obtido %d", http.StatusOK, w.Result().StatusCode)
	}

	var body EvaluationResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&body); err != nil {
		t.Fatalf("erro ao decodificar resposta: %v", err)
	}

	if body.FlagName != "enable-dashboard" {
		t.Fatalf("flag esperada enable-dashboard, obtido %s", body.FlagName)
	}

	if body.UserID != "u1" {
		t.Fatalf("user esperado u1, obtido %s", body.UserID)
	}

	if body.Result != true {
		t.Fatalf("resultado esperado true, obtido %v", body.Result)
	}
}

func TestGetCombinedFlagInfo_CacheHit(t *testing.T) {
	rdb, cleanup := newTestRedis(t)
	defer cleanup()

	info := CombinedFlagInfo{
		Flag: &Flag{
			ID:        1,
			Name:      "flag-cache",
			IsEnabled: true,
		},
		Rule: &TargetingRule{
			ID:        1,
			FlagName:  "flag-cache",
			IsEnabled: true,
			Rules: Rule{
				Type:  "PERCENTAGE",
				Value: 100.0,
			},
		},
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("erro ao serializar info: %v", err)
	}

	err = rdb.Set(ctx, "flag_info:flag-cache", data, CACHE_TTL).Err()
	if err != nil {
		t.Fatalf("erro ao salvar cache: %v", err)
	}

	app := &App{
		RedisClient:         rdb,
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL:      "http://invalid",
		TargetingServiceURL: "http://invalid",
	}

	got, err := app.getCombinedFlagInfo("flag-cache")
	if err != nil {
		t.Fatalf("não esperava erro: %v", err)
	}

	if got.Flag == nil || got.Flag.Name != "flag-cache" {
		t.Fatal("cache hit não retornou a flag esperada")
	}
}

func TestGetCombinedFlagInfo_CacheMissAndStore(t *testing.T) {
	rdb, cleanup := newTestRedis(t)
	defer cleanup()

	flagSrv := newJSONServer(t, http.StatusOK, Flag{
		ID:        1,
		Name:      "flag-miss",
		IsEnabled: true,
	})
	defer flagSrv.Close()

	targetingSrv := newJSONServer(t, http.StatusOK, TargetingRule{
		ID:        1,
		FlagName:  "flag-miss",
		IsEnabled: true,
		Rules: Rule{
			Type:  "PERCENTAGE",
			Value: 50.0,
		},
	})
	defer targetingSrv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		RedisClient:         rdb,
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL:      flagSrv.URL,
		TargetingServiceURL: targetingSrv.URL,
	}

	got, err := app.getCombinedFlagInfo("flag-miss")
	if err != nil {
		t.Fatalf("não esperava erro: %v", err)
	}

	if got.Flag == nil || got.Flag.Name != "flag-miss" {
		t.Fatal("flag não retornada corretamente")
	}

	cached, err := rdb.Get(ctx, "flag_info:flag-miss").Result()
	if err != nil {
		t.Fatalf("esperava item no cache, erro: %v", err)
	}
	if cached == "" {
		t.Fatal("cache deveria ter sido preenchido")
	}
}

func TestFetchFlag_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		HttpClient:     &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL: srv.URL,
	}

	_, err := app.fetchFlag("flag-x")
	if err == nil {
		t.Fatal("esperava erro")
	}

	if _, ok := err.(*NotFoundError); !ok {
		t.Fatalf("esperava NotFoundError, obtido %T", err)
	}
}

func TestFetchRule_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		TargetingServiceURL: srv.URL,
	}

	_, err := app.fetchRule("flag-x")
	if err == nil {
		t.Fatal("esperava erro")
	}

	if _, ok := err.(*NotFoundError); !ok {
		t.Fatalf("esperava NotFoundError, obtido %T", err)
	}
}

func TestRunEvaluationLogic_FlagDisabled(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: false,
		},
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != false {
		t.Fatalf("esperava false, obtido %v", result)
	}
}

func TestRunEvaluationLogic_NoRuleReturnsTrueWhenFlagEnabled(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: true,
		},
		Rule: nil,
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != true {
		t.Fatalf("esperava true, obtido %v", result)
	}
}

func TestRunEvaluationLogic_RuleDisabledReturnsTrue(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: true,
		},
		Rule: &TargetingRule{
			IsEnabled: false,
		},
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != true {
		t.Fatalf("esperava true, obtido %v", result)
	}
}

func TestRunEvaluationLogic_Percentage100ReturnsTrue(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: true,
		},
		Rule: &TargetingRule{
			IsEnabled: true,
			Rules: Rule{
				Type:  "PERCENTAGE",
				Value: 100.0,
			},
		},
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != true {
		t.Fatalf("esperava true, obtido %v", result)
	}
}

func TestRunEvaluationLogic_InvalidPercentageTypeReturnsFalse(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: true,
		},
		Rule: &TargetingRule{
			IsEnabled: true,
			Rules: Rule{
				Type:  "PERCENTAGE",
				Value: "invalido",
			},
		},
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != false {
		t.Fatalf("esperava false, obtido %v", result)
	}
}

func TestGetDeterministicBucket_IsDeterministic(t *testing.T) {
	a := getDeterministicBucket("user1flagA")
	b := getDeterministicBucket("user1flagA")

	if a != b {
		t.Fatalf("bucket deveria ser determinístico: %d != %d", a, b)
	}

	if a < 0 || a > 99 {
		t.Fatalf("bucket fora do intervalo esperado: %d", a)
	}
}

func TestSendEvaluationEvent_SQSDisabled(t *testing.T) {
	app := &App{
		SqsSvc:      nil,
		SqsQueueURL: "",
	}

	app.sendEvaluationEvent("user1", "flagA", true)
}

func TestNotFoundError_Error(t *testing.T) {
	err := &NotFoundError{FlagName: "flag-x"}
	msg := err.Error()

	if !strings.Contains(msg, "flag-x") {
		t.Fatalf("mensagem deveria conter o nome da flag, obtido: %s", msg)
	}
}

func TestGetCombinedFlagInfo_CacheHit_InvalidJSONFallsBackToServices(t *testing.T) {
	rdb, cleanup := newTestRedis(t)
	defer cleanup()

	err := rdb.Set(ctx, "flag_info:flag-bad-json", "{invalid-json", CACHE_TTL).Err()
	if err != nil {
		t.Fatalf("erro ao salvar cache inválido: %v", err)
	}

	flagSrv := newJSONServer(t, http.StatusOK, Flag{
		ID:        1,
		Name:      "flag-bad-json",
		IsEnabled: true,
	})
	defer flagSrv.Close()

	targetingSrv := newJSONServer(t, http.StatusOK, TargetingRule{
		ID:        1,
		FlagName:  "flag-bad-json",
		IsEnabled: true,
		Rules: Rule{
			Type:  "PERCENTAGE",
			Value: 100.0,
		},
	})
	defer targetingSrv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		RedisClient:         rdb,
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL:      flagSrv.URL,
		TargetingServiceURL: targetingSrv.URL,
	}

	got, err := app.getCombinedFlagInfo("flag-bad-json")
	if err != nil {
		t.Fatalf("não esperava erro: %v", err)
	}

	if got.Flag == nil || got.Flag.Name != "flag-bad-json" {
		t.Fatal("fallback para serviços não funcionou após cache inválido")
	}
}

func TestFetchFlag_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "erro interno", http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		HttpClient:     &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL: srv.URL,
	}

	_, err := app.fetchFlag("flag-x")
	if err == nil {
		t.Fatal("esperava erro")
	}
}

func TestFetchRule_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "erro interno", http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		TargetingServiceURL: srv.URL,
	}

	_, err := app.fetchRule("flag-x")
	if err == nil {
		t.Fatal("esperava erro")
	}
}

func TestFetchFlag_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{invalid-json"))
	}))
	defer srv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		HttpClient:     &http.Client{Timeout: 2 * time.Second},
		FlagServiceURL: srv.URL,
	}

	_, err := app.fetchFlag("flag-x")
	if err == nil {
		t.Fatal("esperava erro")
	}
}

func TestFetchRule_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{invalid-json"))
	}))
	defer srv.Close()

	t.Setenv("SERVICE_API_KEY", "service-key")

	app := &App{
		HttpClient:          &http.Client{Timeout: 2 * time.Second},
		TargetingServiceURL: srv.URL,
	}

	_, err := app.fetchRule("flag-x")
	if err == nil {
		t.Fatal("esperava erro")
	}
}

func TestRunEvaluationLogic_PercentageZeroReturnsFalse(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: true,
		},
		Rule: &TargetingRule{
			IsEnabled: true,
			Rules: Rule{
				Type:  "PERCENTAGE",
				Value: 0.0,
			},
		},
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != false {
		t.Fatalf("esperava false, obtido %v", result)
	}
}

func TestRunEvaluationLogic_UnknownRuleTypeReturnsFalse(t *testing.T) {
	app := &App{}

	info := &CombinedFlagInfo{
		Flag: &Flag{
			Name:      "flag-a",
			IsEnabled: true,
		},
		Rule: &TargetingRule{
			IsEnabled: true,
			Rules: Rule{
				Type:  "UNKNOWN",
				Value: 50.0,
			},
		},
	}

	result := app.runEvaluationLogic(info, "user1")
	if result != false {
		t.Fatalf("esperava false, obtido %v", result)
	}
}