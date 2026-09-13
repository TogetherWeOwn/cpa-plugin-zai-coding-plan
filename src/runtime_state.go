package main

import (
	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/coordinator"
	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/providers/opencodego"
	"github.com/TogetherWeOwn/cpa-plugin-zai-coding-plan/internal/providers/zai"
)

var runtimeState = coordinator.New(zai.NewModule(), opencodego.NewModule())
