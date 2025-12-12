package utils

import (
	yaml "gopkg.in/yaml.v2"
	"k8s.io/apimachinery/pkg/runtime"
)

func StructToYaml(obj interface{}) (string, error) {
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&obj)
	if err != nil {
		return "", err
	}

	delete(unstructuredObj["metadata"].(map[string]interface{}), "managedFields")

	yamlBytes, err := yaml.Marshal(unstructuredObj)
	if err != nil {
		return "", err
	}
	return string(yamlBytes), nil
}
