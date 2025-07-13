// main.go
package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"
	"crypto/tls"
	"net"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

//go:embed frontend/*
var frontendFiles embed.FS

type Service struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Endpoint  string    `json:"endpoint"` // URL for HTTP, "host:port" for TCP
	Type      string    `json:"type"`     // "http", "https", "tcp"
	Status    string    `json:"status"`   // "up", "down", "unknown"
	Latency   int       `json:"latency"`
	CheckedAt time.Time `json:"checkedAt"`
}

type ServiceStore struct {
	mu       sync.RWMutex
	services map[string]*Service
}

func NewServiceStore() *ServiceStore {
	return &ServiceStore{
		services: make(map[string]*Service),
	}
}

func (s *ServiceStore) AddService(service *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[service.ID] = service
}

func (s *ServiceStore) GetAll() []*Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	services := make([]*Service, 0, len(s.services))
	for _, svc := range s.services {
		services = append(services, svc)
	}
	return services
}

func (s *ServiceStore) GetByID(id string) (*Service, bool) {
  s.mu.RLock()
  defer s.mu.RUnlock()
  svc, exists := s.services[id]
  return svc, exists
}

func (s *ServiceStore) UpdateStatus(id, status string, latency int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if service, exists := s.services[id]; exists {
		service.Status = status
		service.Latency = latency
		service.CheckedAt = time.Now()
		return true
	}
	return false
}

type HealthChecker struct {
	store      *ServiceStore
	broker     *EventBroker
	interval   time.Duration
	shutdownCh chan struct{}
}

func NewHealthChecker(store *ServiceStore, broker *EventBroker, interval time.Duration) *HealthChecker {
	return &HealthChecker{
		store:      store,
		broker:     broker,
		interval:   interval,
		shutdownCh: make(chan struct{}),
	}
}

func (hc *HealthChecker) Start() {
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()
	firstCheck := make(chan struct{}, 1)
	firstCheck <- struct{}{}
	
	for {
		select {
		case <-firstCheck:
			log.Println("running first checks")
			services := hc.store.GetAll()
			for _, svc := range services {
				go hc.checkService(svc)
			}
		case <-ticker.C:
			services := hc.store.GetAll()
			for _, svc := range services {
				go hc.checkService(svc)
			}
		case <-hc.shutdownCh:
			return
		}
	}
}

func (hc *HealthChecker) Stop() {
	close(hc.shutdownCh)
}

func (hc *HealthChecker) checkService(service *Service) {
	// start := time.Now()
  status, latency := "unknown", 0

	switch strings.ToLower(service.Type) {
	case "http", "https":
		status, latency = hc.checkHTTP(service.Endpoint)
	case "tcp":
		status, latency = hc.checkTCP(service.Endpoint)
	default:
		status = "unknown"
		latency = 0
	}

	if hc.store.UpdateStatus(service.ID, status, latency) {
    if updated, exists := hc.store.GetByID(service.ID); exists {
      if data, err := json.Marshal(updated); err == nil {
        hc.broker.Publish(data)
      }
    }
  }
}

func (hc *HealthChecker) checkHTTP(url string) (string, int) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	start := time.Now()
	resp, err := client.Get(url)
	latency := int(time.Since(start).Milliseconds())

	if err != nil {
		return "down", latency
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return "up", latency
	}
	return "down", latency
}

func (hc *HealthChecker) checkTCP(endpoint string) (string, int) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	latency := int(time.Since(start).Milliseconds())
	
	if err != nil {
		return "down", latency
	}
	defer conn.Close()
	return "up", latency
}

// EventBroker manages real-time client connections
type EventBroker struct {
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
}

func NewEventBroker() *EventBroker {
	return &EventBroker{
		subscribers: make(map[chan []byte]struct{}),
	}
}

func (b *EventBroker) Subscribe() chan []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	ch := make(chan []byte, 10)
	b.subscribers[ch] = struct{}{}
	return ch
}

func (b *EventBroker) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	delete(b.subscribers, ch)
	close(ch)
}

func (b *EventBroker) Publish(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	for ch := range b.subscribers {
		select {
		case ch <- data:
		default:
			// Prevent blocking on slow consumers
		}
	}
}

func main() {
	e := echo.New()
	e.Use(middleware.CORS())
	e.Use(middleware.Logger())


	frontendContent, err := fs.Sub(frontendFiles, "frontend")
	if err != nil {
		log.Fatal(err)
	}
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(frontendContent))))
	
	store := NewServiceStore()
	broker := NewEventBroker()
	
	store.AddService(&Service{ID: "1", Name: "Jellyfin", Endpoint: "http://127.0.0.1:8096", Type: "http"})
	store.AddService(&Service{
		ID:       "2",
		Name:     "Google",
		Endpoint: "https://www.google.com",
		Type:     "https",
	})
	
	store.AddService(&Service{
		ID:       "3",
		Name:     "Cloudflare DNS",
		Endpoint: "1.1.1.1:53",
		Type:     "tcp",
	})
	
	checker := NewHealthChecker(store, broker, 2*time.Second)

	go checker.Start()
	defer checker.Stop()
	
	// API Endpoints
	e.GET("/api/services", func(c echo.Context) error {
		return c.JSON(http.StatusOK, store.GetAll())
	})
	
	e.GET("/api/events", func(c echo.Context) error {
		ch := broker.Subscribe()
		defer broker.Unsubscribe(ch)
		
		c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
		c.Response().Header().Set("Cache-Control", "no-cache")
		c.Response().Header().Set("Connection", "keep-alive")
		c.Response().WriteHeader(http.StatusOK)
		
		for {
			select {
			case data := <-ch:
				if _, err := c.Response().Write([]byte("data: ")); err != nil {
					return err
				}
				if _, err := c.Response().Write(data); err != nil {
					return err
				}
				if _, err := c.Response().Write([]byte("\n\n")); err != nil {
					return err
				}
				c.Response().Flush()
			case <-c.Request().Context().Done():
				return nil
			}
		}
	})
	
	// Start API server
	log.Fatal(e.Start(":3030"))
}
