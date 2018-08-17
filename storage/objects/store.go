package objects

import (
	pelotonstore "code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/storage/cassandra"
	escassandra "code.uber.internal/infra/peloton/storage/connectors/cassandra"
	"code.uber.internal/infra/peloton/storage/orm"

	"github.com/uber-go/tally"
)

// Store contains ORM client as well as metrics
type Store struct {
	oClient orm.Client
	metrics *pelotonstore.Metrics
}

// NewCassandraStore creates a new Cassandra storage client
func NewCassandraStore(
	config *cassandra.Config, scope tally.Scope) (*Store, error) {
	connector, err := escassandra.NewCassandraConnector(config, scope)
	if err != nil {
		return nil, err
	}
	// TODO: Load up all objects automatically instead of explicitly adding
	// them here. Might need to add some Go init() magic to do this.
	oclient, err := orm.NewClient(
		connector, &SecretObject{})
	if err != nil {
		return nil, err
	}
	return &Store{
		oClient: oclient,
		metrics: pelotonstore.NewMetrics(scope),
	}, nil
}
