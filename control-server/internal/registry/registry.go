package registry

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"tunnel-control/internal/models"
)

type ServiceRegistry interface {
	Register(service *models.Service) error
	Deregister(serviceID string) error
	Get(serviceID string) (*models.Service, error)
	GetAll() ([]models.Service, error)
	FindByTag(key, value string) ([]models.Service, error)
	UpdateHeartbeat(serviceID string) error
	CleanupStaleServices(timeout time.Duration) []string
}

type InMemoryRegistry struct {
	services map[string]*models.Service
	mu       sync.RWMutex
}

func NewInMemoryRegistry() *InMemoryRegistry {
	return &InMemoryRegistry{
		services: make(map[string]*models.Service),
	}
}

func (r *InMemoryRegistry) Register(service *models.Service) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.services[service.ID]; exists {
		return fmt.Errorf("service %s already registered", service.ID)
	}

	service.Status = "online"
	service.LastSeen = time.Now()
	service.ConnectedAt = time.Now()

	r.services[service.ID] = service

	// Log registration
	data, _ := json.MarshalIndent(service, "", "  ")
	fmt.Printf("Service registered: %s\n%s\n", service.ID, string(data))

	return nil
}

func (r *InMemoryRegistry) Deregister(serviceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.services[serviceID]; !exists {
		return fmt.Errorf("service %s not found", serviceID)
	}

	delete(r.services, serviceID)
	fmt.Printf("Service deregistered: %s\n", serviceID)

	return nil
}

func (r *InMemoryRegistry) Get(serviceID string) (*models.Service, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	service, exists := r.services[serviceID]
	if !exists {
		return nil, fmt.Errorf("service %s not found", serviceID)
	}

	return service, nil
}

func (r *InMemoryRegistry) GetAll() ([]models.Service, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	services := make([]models.Service, 0, len(r.services))
	for _, s := range r.services {
		// Create a copy without the connection
		serviceCopy := *s
		serviceCopy.Conn = nil
		services = append(services, serviceCopy)
	}

	return services, nil
}

func (r *InMemoryRegistry) FindByTag(key, value string) ([]models.Service, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var matched []models.Service
	for _, s := range r.services {
		if tagValue, exists := s.Tags[key]; exists && tagValue == value {
			serviceCopy := *s
			serviceCopy.Conn = nil
			matched = append(matched, serviceCopy)
		}
	}

	return matched, nil
}

func (r *InMemoryRegistry) UpdateHeartbeat(serviceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	service, exists := r.services[serviceID]
	if !exists {
		return fmt.Errorf("service %s not found", serviceID)
	}

	service.LastSeen = time.Now()
	return nil
}

func (r *InMemoryRegistry) CleanupStaleServices(timeout time.Duration) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var removed []string
	now := time.Now()

	for id, service := range r.services {
		if now.Sub(service.LastSeen) > timeout {
			removed = append(removed, id)
			delete(r.services, id)

			// Notify via WebSocket if connection exists
			if conn, ok := service.Conn.(interface{ WriteMessage(int, []byte) error }); ok {
				msg := models.Message{
					Type: "connection_closed",
					Payload: map[string]string{
						"reason": "heartbeat_timeout",
					},
				}
				data, _ := json.Marshal(msg)
				conn.WriteMessage(1, data)
			}
		}
	}

	return removed
}
