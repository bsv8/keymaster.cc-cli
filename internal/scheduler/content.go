package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// contentStore is a JSON object that preserves insertion order, matching the
// TypeScript kernel's `Object.values(state.content)` iteration behaviour.
type contentStore struct {
	entries map[string]Content
	order   []string
}

func newContentStore() contentStore {
	return contentStore{entries: map[string]Content{}}
}

func (c *contentStore) ensure() {
	if c.entries == nil {
		c.entries = map[string]Content{}
	}
}

// Get returns one content item.
func (c *contentStore) Get(id string) (Content, bool) {
	if c == nil || c.entries == nil {
		return Content{}, false
	}
	item, ok := c.entries[id]
	return item, ok
}

// Set inserts or replaces an item; a new id is appended to the order.
func (c *contentStore) Set(item Content) {
	c.ensure()
	if _, ok := c.entries[item.ID]; !ok {
		c.order = append(c.order, item.ID)
	}
	c.entries[item.ID] = item
}

// Values returns items in insertion order.
func (c *contentStore) Values() []Content {
	if c == nil {
		return nil
	}
	values := make([]Content, 0, len(c.order))
	for _, id := range c.order {
		if item, ok := c.entries[id]; ok {
			values = append(values, item)
		}
	}
	return values
}

// Len returns the number of items.
func (c *contentStore) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// MarshalJSON writes the object in insertion order.
func (c contentStore) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	written := make(map[string]bool, len(c.order))
	first := true
	write := func(item Content) error {
		key, err := json.Marshal(item.ID)
		if err != nil {
			return err
		}
		value, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if !first {
			buffer.WriteByte(',')
		}
		first = false
		buffer.Write(key)
		buffer.WriteByte(':')
		buffer.Write(value)
		return nil
	}
	for _, id := range c.order {
		item, ok := c.entries[id]
		if !ok {
			continue
		}
		if err := write(item); err != nil {
			return nil, err
		}
		written[id] = true
	}
	// Keep unknown entries (defensive; restore validation rejects mismatches).
	for id, item := range c.entries {
		if written[id] {
			continue
		}
		if err := write(item); err != nil {
			return nil, err
		}
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// UnmarshalJSON captures the JSON key order.
func (c *contentStore) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("content must be a JSON object")
	}
	c.entries = map[string]Content{}
	c.order = nil
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("content key must be a string")
		}
		var item Content
		if err := decoder.Decode(&item); err != nil {
			return err
		}
		if _, exists := c.entries[key]; !exists {
			c.order = append(c.order, key)
		}
		c.entries[key] = item
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}
