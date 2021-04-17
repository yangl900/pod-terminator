module github.com/yangl900/pod-terminator

go 1.15

require (
	github.com/stretchr/testify v1.7.0 // indirect
	k8s.io/api v0.21.0
	k8s.io/apimachinery v0.21.0
	k8s.io/client-go v0.20.4
	k8s.io/klog v1.0.0
)

replace k8s.io/client-go => k8s.io/client-go v0.21.0
