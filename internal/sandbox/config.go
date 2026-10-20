package sandbox

import (
	"errors"
	"time"
)

func normalizePoolConfig(c *Config) error {
	if c.Mode == "" {
		c.Mode = "cold"
	}
	if c.Mode != "cold" && c.Mode != "warm" {
		return errors.New("mode must be cold or warm")
	}
	if c.Mode == "cold" {
		if c.DynamicWarmPool {
			return errors.New("dynamic inventory requires warm mode")
		}
		return nil
	}
	if c.WarmPoolSize == 0 {
		c.WarmPoolSize = 3
	}
	if c.MaxSandboxes == 0 {
		c.MaxSandboxes = 6
	}
	if c.CreateConcurrency == 0 {
		c.CreateConcurrency = 1
	}
	if c.QueueSize == 0 {
		c.QueueSize = 16
	}
	if c.QueueBytes == 0 {
		c.QueueBytes = 8 << 20
	}
	if c.QueueTimeout == 0 {
		c.QueueTimeout = 100 * time.Millisecond
	}
	if c.WarmPoolSize < 1 || c.WarmPoolSize > 8 || c.MaxSandboxes > 8 || c.MaxSandboxes < c.Concurrency || c.MaxSandboxes < 1 || (!c.DynamicWarmPool && c.WarmPoolSize > c.MaxSandboxes) {
		return errors.New("warm pool requires 1 <= warm-size <= max-sandboxes <= 8 and max-sandboxes >= concurrency")
	}
	maxInventory := c.WarmPoolSize
	if c.DynamicWarmPool {
		if c.WarmMinSize == 0 {
			c.WarmMinSize = 1
		}
		if c.WarmMaxSize == 0 {
			c.WarmMaxSize = c.WarmPoolSize
		}
		if c.WarmAdjustInterval == 0 {
			c.WarmAdjustInterval = time.Second
		}
		if c.WarmDownscaleWindow == 0 {
			c.WarmDownscaleWindow = 30 * time.Second
		}
		if c.WarmMinSize < 1 || c.WarmMinSize > c.WarmMaxSize || c.WarmMaxSize > c.MaxSandboxes {
			return errors.New("dynamic inventory requires 1 <= warm-min-size <= warm-max-size <= max-sandboxes")
		}
		if c.WarmAdjustInterval < 100*time.Millisecond || c.WarmAdjustInterval > time.Minute || c.WarmDownscaleWindow < c.WarmAdjustInterval || c.WarmDownscaleWindow > time.Hour {
			return errors.New("warm-adjust-interval must be 100ms..1m and warm-downscale-window must be between that interval and 1h")
		}
		maxInventory = c.WarmMaxSize
	}
	if c.CreateConcurrency < 1 || c.CreateConcurrency > 2 || c.CreateConcurrency > maxInventory {
		return errors.New("create-concurrency must be 1 or 2 and not exceed the READY target upper bound")
	}
	if c.QueueSize < 1 || c.QueueSize > 128 || c.QueueBytes < 1 || c.QueueBytes > 64<<20 {
		return errors.New("queue-size must be 1..128 and queue-bytes must be 1..64 MiB")
	}
	if c.QueueTimeout < time.Millisecond || c.QueueTimeout > 5*time.Second {
		return errors.New("queue-timeout must be 1ms..5s")
	}
	return nil
}
