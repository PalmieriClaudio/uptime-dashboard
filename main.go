// main.go
package main

import (
	"fmt"
	"database/sql"
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
	_ "github.com/mattn/go-sqlite3"
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
	db       *sql.DB
}

func NewServiceStore(db *sql.DB) *ServiceStore {
	store := &ServiceStore{
		services: make(map[string]*Service),
		db:       db,
	}
	store.loadFromDB()
	return store
}

func (s *ServiceStore) loadFromDB() {
	rows, err := s.db.Query("SELECT id, name, endpoint, type, status, latency, checked_at FROM services")
	if err != nil {
		log.Printf("Error loading services: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var svc Service
		err := rows.Scan(&svc.ID, &svc.Name, &svc.Endpoint, &svc.Type, &svc.Status, &svc.Latency, &svc.CheckedAt)
		if err != nil {
			log.Printf("Error scanning service: %v", err)
			continue
		}
		s.services[svc.ID] = &svc
	}
}

func (s *ServiceStore) AddService(service *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO services 
		(id, name, endpoint, type, status, latency, checked_at) 
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		service.ID, service.Name, service.Endpoint, service.Type, 
		service.Status, service.Latency, service.CheckedAt)
	
	if err != nil {
		log.Printf("Error saving service: %v", err)
		return
	}
	
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
      // Update current status
      service.Status = status
      service.Latency = latency
      service.CheckedAt = time.Now()
      
      // Record history
      _, err := s.db.Exec(`
        INSERT INTO service_history 
        (service_id, status, latency, checked_at)
        VALUES (?, ?, ?, ?)`,
        id, status, latency, time.Now())
      
      if err != nil {
        log.Printf("Error saving history: %v", err)
      }
      
      return true
    }
    return false
}

func (s *ServiceStore) DeleteService(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	_, err := s.db.Exec("DELETE FROM services WHERE id = ?", id)
	if err != nil {
		log.Printf("Error deleting service: %v", err)
	}
	
	delete(s.services, id)
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
	// Initialize SQLite database
	db, err := sql.Open("sqlite3", "./services.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create services table if not exists
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS services (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			endpoint TEXT NOT NULL,
			type TEXT NOT NULL,
			status TEXT,
			latency INTEGER,
			checked_at DATETIME
		)
	`)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(`
    CREATE TABLE IF NOT EXISTS service_history (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        service_id TEXT NOT NULL,
        status TEXT NOT NULL,
        latency INTEGER NOT NULL,
        checked_at DATETIME NOT NULL,
        FOREIGN KEY(service_id) REFERENCES services(id)
    );
    
    CREATE INDEX IF NOT EXISTS idx_service_history ON service_history(service_id, checked_at);
	`)
	if err != nil {
		log.Fatal(err)
	}

	e := echo.New()
	e.Use(middleware.CORS())
	e.Use(middleware.Logger())

	frontendContent, err := fs.Sub(frontendFiles, "frontend")
	if err != nil {
		log.Fatal(err)
	}
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(frontendContent))))
	
	store := NewServiceStore(db)
	broker := NewEventBroker()

	// Only add default services if DB is empty
	if len(store.GetAll()) == 0 {
		store.AddService(&Service{
			ID:       "1",
			Name:     "Jellyfin",
			Endpoint: "http://127.0.0.1:8096",
			Type:     "http",
		})
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
	}
	
	checker := NewHealthChecker(store, broker, 2*time.Second)
	go checker.Start()
	defer checker.Stop()
	
	// API Endpoints
	e.GET("/api/services", func(c echo.Context) error {
		return c.JSON(http.StatusOK, store.GetAll())
	})
	
	e.POST("/api/services", func(c echo.Context) error {
		var newService Service
		if err := c.Bind(&newService); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
		}
		
		if newService.ID == "" {
			newService.ID = generateUUID() 
		}
		
		store.AddService(&newService)
		go checker.checkService(&newService)
		
		return c.JSON(http.StatusCreated, newService)
	})

	e.GET("/api/services/:id", func(c echo.Context) error {
    id := c.Param("id")
    if svc, exists := store.GetByID(id); exists {
        return c.JSON(http.StatusOK, svc)
    }
    return c.JSON(http.StatusNotFound, map[string]string{"error": "Service not found"})
	})

	e.PUT("/api/services/:id", func(c echo.Context) error {
		id := c.Param("id")
		var updatedService Service
		if err := c.Bind(&updatedService); err != nil {
			return err
		}
		
		if existing, ok := store.GetByID(id); ok {
			existing.Name = updatedService.Name
			existing.Endpoint = updatedService.Endpoint
			existing.Type = updatedService.Type
			store.AddService(existing) 
			go checker.checkService(existing)
			return c.JSON(http.StatusOK, existing)
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Service not found"})
	})
	
	e.DELETE("/api/services/:id", func(c echo.Context) error {
		id := c.Param("id")
		if _, ok := store.GetByID(id); ok {
			store.DeleteService(id)
			return c.NoContent(http.StatusNoContent)
		}
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Service not found"})
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

	e.GET("/api/services/:id/history", func(c echo.Context) error {
    id := c.Param("id")
    hours := c.QueryParam("hours")
    if hours == "" {
      hours = "24" // Default to 24 hours
    }
    
    rows, err := db.Query(`
      SELECT status, latency, checked_at 
      FROM service_history 
      WHERE service_id = ? 
      AND checked_at >= datetime('now', ? || ' hours')
      ORDER BY checked_at`,
      id, "-"+hours)
    
    if err != nil {
      return err
    }
    defer rows.Close()
    
    var history []map[string]interface{}
    for rows.Next() {
      var status string
      var latency int
      var checkedAt time.Time
      
      err := rows.Scan(&status, &latency, &checkedAt)
      if err != nil {
        continue
      }
      
      history = append(history, map[string]interface{}{
        "status":    status,
        "latency":   latency,
        "checkedAt": checkedAt.Format(time.RFC3339),
      })
    }
    
    return c.JSON(http.StatusOK, history)
})
	
	log.Fatal(e.Start(":3030"))
}

func generateUUID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano()) // This is not good enough but will do for a local dashboard with one user at a time.
}
