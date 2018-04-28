package xgorm_test

import (
	"testing"

	"github.com/LiuRoy/xgorm"
	"context"
)

type CalculateField struct {
	xgorm.Model
	Name     string
	Children []CalculateFieldChild
	Category CalculateFieldCategory
	EmbeddedField
}

type EmbeddedField struct {
	EmbeddedName string `sql:"NOT NULL;DEFAULT:'hello'"`
}

type CalculateFieldChild struct {
	xgorm.Model
	CalculateFieldID uint
	Name             string
}

type CalculateFieldCategory struct {
	xgorm.Model
	CalculateFieldID uint
	Name             string
}

func TestCalculateField(t *testing.T) {
	var field CalculateField
	var scope = DB.NewScope(context.Background(), &field)
	if field, ok := scope.FieldByName("Children"); !ok || field.Relationship == nil {
		t.Errorf("Should calculate fields correctly for the first time")
	}

	if field, ok := scope.FieldByName("Category"); !ok || field.Relationship == nil {
		t.Errorf("Should calculate fields correctly for the first time")
	}

	if field, ok := scope.FieldByName("embedded_name"); !ok {
		t.Errorf("should find embedded field")
	} else if _, ok := field.TagSettings["NOT NULL"]; !ok {
		t.Errorf("should find embedded field's tag settings")
	}
}
