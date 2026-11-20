package service

import "github.com/kpenfound/osmia/internal/checks"

type ReviewChecks = checks.ReviewChecks
type DaggerChecks = checks.DaggerChecks

type CloudRerunner = checks.CloudRerunner

const ChecksPassed = checks.ChecksPassed
const ChecksFailed = checks.ChecksFailed
const ChecksIncomplete = checks.ChecksIncomplete
const ChecksSuperseded = checks.ChecksSuperseded
