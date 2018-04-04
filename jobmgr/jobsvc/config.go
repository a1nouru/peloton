package jobsvc

const (
	_defaultMaxTasksPerJob uint32 = 100000
)

// Config for job service
type Config struct {
	// Maximum number of tasks allowed per job
	MaxTasksPerJob uint32 `yaml:"max_tasks_per_job"`
}

func (c *Config) normalize() {
	if c.MaxTasksPerJob == 0 {
		c.MaxTasksPerJob = _defaultMaxTasksPerJob
	}
}