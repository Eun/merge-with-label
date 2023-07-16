package go_gen_graphql

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// InvalidTypeError indicates that the type is invalid or not supported.
type InvalidTypeError struct{}

func (InvalidTypeError) Error() string {
	return "invalid type"
}

// Options can be used to modify the behavior of
// Generate, Generatef and GenerateFromReflectValue.
type Options struct {
	GraphQLTag string
	JSONTag    string
	Indent     string
}

// Generate generates a graphql body from a struct.
// Returns the graphql body and error.
func Generate(in any, options *Options) (string, error) {
	if in == nil {
		return "", errors.New("in is invalid")
	}
	return GenerateFromReflectValue(reflect.TypeOf(in), options)
}

// Generatef generates a graphql body from a struct.
// Returns the graphql body and error.
func Generatef(in any, options *Options, a ...any) (string, error) {
	s, err := Generate(in, options)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(s, a...), nil
}

// GenerateFromReflectValue generates a graphql body from reflect Type.
// Returns the graphql body and error.
func GenerateFromReflectValue(v reflect.Type, options *Options) (string, error) {
	if options == nil {
		options = &Options{}
	}

	jsonTagName := options.JSONTag
	if jsonTagName == "" {
		jsonTagName = "json"
	}
	graphQLTagName := options.GraphQLTag
	if graphQLTagName == "" {
		graphQLTagName = "graphql"
	}
	indent := options.Indent
	if indent == "" {
		indent = "  "
	}

	var sb strings.Builder
	if err := generateForStruct(&sb, v, 0, &indent, &jsonTagName, &graphQLTagName); err != nil {
		return "", fmt.Errorf("unable to create: %w", err)
	}
	return sb.String(), nil
}

func generateForStruct(w io.Writer, v reflect.Type, indentLevel int, indent, jsonTagName, graphQLTagName *string) error {
	if v.Kind() == reflect.Invalid {
		return InvalidTypeError{}
	}
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() == reflect.Invalid {
		return InvalidTypeError{}
	}
	if v.Kind() != reflect.Struct {
		return InvalidTypeError{}
	}
	firstLine := 0
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if !field.IsExported() {
			continue
		}
		name := field.Tag.Get(*graphQLTagName)
		if name == "-" {
			firstLine++
			continue
		}
		if name == "" {
			name = field.Tag.Get(*jsonTagName)
			name, _, _ = strings.Cut(name, ",")
		}
		if name == "" {
			name = field.Name
		}
		if i > firstLine {
			_, err := w.Write([]byte{'\n'})
			if err != nil {
				return fmt.Errorf("error writing: %w", err)
			}
		}
		if err := writeIndent(w, indent, indentLevel); err != nil {
			return fmt.Errorf("error writing: %w", err)
		}
		if _, err := io.WriteString(w, name); err != nil {
			return fmt.Errorf("error writing: %w", err)
		}

		fieldType := field.Type

		if fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}

		if fieldType.Kind() == reflect.Slice {
			fieldType = fieldType.Elem()
		}

		if fieldType.Kind() == reflect.Struct {
			if _, err := w.Write([]byte{'{', '\n'}); err != nil {
				return fmt.Errorf("error writing: %w", err)
			}
			if err := generateForStruct(w, fieldType, indentLevel+1, indent, jsonTagName, graphQLTagName); err != nil {
				return fmt.Errorf("unable to generate for nested child %q: %w", field.Name, err)
			}

			if _, err := w.Write([]byte{'\n'}); err != nil {
				return fmt.Errorf("error writing: %w", err)
			}
			if err := writeIndent(w, indent, indentLevel); err != nil {
				return fmt.Errorf("error writing: %w", err)
			}

			if _, err := w.Write([]byte{'}'}); err != nil {
				return fmt.Errorf("error writing: %w", err)
			}
		}
	}
	return nil
}

func writeIndent(w io.Writer, indent *string, indentLevel int) error {
	for i := 0; i < indentLevel; i++ {
		_, err := io.WriteString(w, *indent)
		if err != nil {
			return fmt.Errorf("error writing: %w", err)
		}
	}
	return nil
}
