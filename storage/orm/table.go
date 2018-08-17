package orm

import (
	"reflect"
	"strings"

	"code.uber.internal/infra/peloton/storage/objects/base"

	"go.uber.org/yarpc/yarpcerrors"
)

// Table is an ORM internal representation of storage object. Storage
// object is translated into Definition that contains the primary key
// information as well as column to datatype map
// It also contains maps used to translate storage object fields into DB columns
// and viceversa and this is used during read and write operations
type Table struct {
	base.Definition

	// map from DB column name -> object field name
	ColToField map[string]string

	// map of base field name to DB column name
	FieldToCol map[string]string
}

// GetKeyRowFromObject is a helper for generating a row of partition and
// clustering key column values to be used in a select query.
func (t *Table) GetKeyRowFromObject(
	e base.Object) []base.Column {
	v := reflect.ValueOf(e).Elem()
	row := []base.Column{}

	// populate partition key values
	for _, pk := range t.Key.PartitionKeys {
		fieldName := t.ColToField[pk]
		value := v.FieldByName(fieldName)
		row = append(row, base.Column{
			Name:  pk,
			Value: value.Interface(),
		})
	}

	// populate clustering key values
	for _, ck := range t.Key.ClusteringKeys {
		fieldName := t.ColToField[ck.Name]
		value := v.FieldByName(fieldName)
		row = append(row, base.Column{
			Name:  ck.Name,
			Value: value.Interface(),
		})
	}

	return row
}

// GetRowFromObject is a helper for generating a row from the storage object
func (t *Table) GetRowFromObject(e base.Object) []base.Column {
	v := reflect.ValueOf(e).Elem()
	row := []base.Column{}
	for columnName, fieldName := range t.ColToField {
		value := v.FieldByName(fieldName)
		row = append(row, base.Column{
			Name:  columnName,
			Value: value.Interface(),
		})
	}
	return row
}

// SetObjectFromRow is a helper for populating storage object from the
// given row
func (t *Table) SetObjectFromRow(
	e base.Object, row []base.Column) {

	columnsMap := make(map[string]interface{})
	for _, column := range row {
		columnsMap[column.Name] = column.Value
	}

	r := reflect.ValueOf(e).Elem()

	for columnName, fieldName := range t.ColToField {
		columnValue := columnsMap[columnName]
		val := r.FieldByName(fieldName)
		var fv reflect.Value
		if columnValue != nil {
			fv = reflect.ValueOf(columnValue)
			val.Set(reflect.Indirect(fv))
		}
	}
}

// TableFromObject creates a orm.Table from a storage.Object
// instance.
func TableFromObject(e base.Object) (*Table, error) {
	var err error
	elem := reflect.TypeOf(e).Elem()

	t := &Table{
		ColToField: map[string]string{},
		FieldToCol: map[string]string{},
		Definition: base.Definition{
			ColumnToType: map[string]reflect.Type{},
		},
	}
	for i := 0; i < elem.NumField(); i++ {
		structField := elem.Field(i)
		name := structField.Name

		if name == objectName {
			// Extract Object tags which have all the connector key information
			tag := strings.TrimSpace(structField.Tag.Get(connectorTag))

			// Parse cassandra specific tag to extract table name and primary
			// key information
			if t.Definition.Name, t.Key, err =
				parseCassandraObjectTag(tag); err != nil {
				return nil, err
			}
		} else {
			// For all other fields of this object, parse the column name tag
			tag := strings.TrimSpace(structField.Tag.Get(columnTag))
			columnName, err := parseNameTag(tag)
			if err != nil {
				return nil, err
			}
			// Keep a column name to data type mapping which will be used when
			// allocating row memory for DB queries.
			t.ColumnToType[columnName] = structField.Type

			// Keep a column name to field name and viceversa mapping so that
			// it is easy to convert table to object and viceversa
			t.ColToField[columnName] = name
			t.FieldToCol[name] = columnName
		}
	}

	if t.Key == nil {
		return nil, yarpcerrors.InternalErrorf(
			"cannot find orm.Object in object %v", e)
	}

	return t, nil
}

// BuildObjectIndex builds an index to map storage object type to its
// Table representation
func BuildObjectIndex(objects []base.Object) (
	map[reflect.Type]*Table, error) {
	objectIndex := make(map[reflect.Type]*Table)
	for _, o := range objects {
		// Map each storage object to its internal Table representation used to
		// translate operations on each object to corresponding UQL statements
		table, err := TableFromObject(o)
		if err != nil {
			return nil, err
		}

		// use base type as internal lookup key
		typ := reflect.TypeOf(o).Elem()

		objectIndex[typ] = table
	}
	return objectIndex, nil
}
