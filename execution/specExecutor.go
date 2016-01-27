// Copyright 2015 ThoughtWorks, Inc.

// This file is part of Gauge.

// Gauge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Gauge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with Gauge.  If not, see <http://www.gnu.org/licenses/>.

package execution

import (
	"errors"
	"fmt"
	"strings"

	"github.com/getgauge/gauge/conn"
	"github.com/getgauge/gauge/execution/result"
	"github.com/getgauge/gauge/formatter"
	"github.com/getgauge/gauge/gauge"
	"github.com/getgauge/gauge/gauge_messages"
	"github.com/getgauge/gauge/logger"
	"github.com/getgauge/gauge/parser"
	"github.com/getgauge/gauge/plugin"
	"github.com/getgauge/gauge/reporter"
	"github.com/getgauge/gauge/runner"
	"github.com/golang/protobuf/proto"
)

type specExecutor struct {
	specification        *gauge.Specification
	dataTableIndex       indexRange
	runner               *runner.TestRunner
	pluginHandler        *plugin.Handler
	currentExecutionInfo *gauge_messages.ExecutionInfo
	specResult           *result.SpecResult
	currentTableRow      int
	consoleReporter      reporter.Reporter
	errMap               *validationErrMaps
}

type indexRange struct {
	start int
	end   int
}

func newSpecExecutor(specToExecute *gauge.Specification, runner *runner.TestRunner, pluginHandler *plugin.Handler, tableRows indexRange, reporter reporter.Reporter, errMaps *validationErrMaps) *specExecutor {
	specExecutor := new(specExecutor)
	specExecutor.initialize(specToExecute, runner, pluginHandler, tableRows, reporter, errMaps)
	return specExecutor
}

func (specExec *specExecutor) initialize(specificationToExecute *gauge.Specification, runner *runner.TestRunner, pluginHandler *plugin.Handler, tableRows indexRange, consoleReporter reporter.Reporter, errMap *validationErrMaps) {
	specExec.specification = specificationToExecute
	specExec.runner = runner
	specExec.pluginHandler = pluginHandler
	specExec.dataTableIndex = tableRows
	specExec.consoleReporter = consoleReporter
	specExec.errMap = errMap
}

func (e *specExecutor) executeBeforeSpecHook() *gauge_messages.ProtoExecutionResult {
	message := &gauge_messages.Message{MessageType: gauge_messages.Message_SpecExecutionStarting.Enum(),
		SpecExecutionStartingRequest: &gauge_messages.SpecExecutionStartingRequest{CurrentExecutionInfo: e.currentExecutionInfo}}
	return e.executeHook(message, e.specResult)
}

func (e *specExecutor) initSpecDataStore() *gauge_messages.ProtoExecutionResult {
	initSpecDataStoreMessage := &gauge_messages.Message{MessageType: gauge_messages.Message_SpecDataStoreInit.Enum(),
		SpecDataStoreInitRequest: &gauge_messages.SpecDataStoreInitRequest{}}
	initResult := executeAndGetStatus(e.runner, initSpecDataStoreMessage)
	if initResult.GetFailed() {
		e.consoleReporter.Error("Spec data store didn't get initialized : %s", initResult.ErrorMessage)
	}
	return initResult
}

func (e *specExecutor) executeAfterSpecHook() *gauge_messages.ProtoExecutionResult {
	message := &gauge_messages.Message{MessageType: gauge_messages.Message_SpecExecutionEnding.Enum(),
		SpecExecutionEndingRequest: &gauge_messages.SpecExecutionEndingRequest{CurrentExecutionInfo: e.currentExecutionInfo}}
	return e.executeHook(message, e.specResult)
}

func (e *specExecutor) executeHook(message *gauge_messages.Message, execTimeTracker result.ExecTimeTracker) *gauge_messages.ProtoExecutionResult {
	e.pluginHandler.NotifyPlugins(message)
	executionResult := executeAndGetStatus(e.runner, message)
	execTimeTracker.AddExecTime(executionResult.GetExecutionTime())
	return executionResult
}

func (specExecutor *specExecutor) getSkippedSpecResult() *result.SpecResult {
	scenarioResults := make([]*result.ScenarioResult, 0)
	for _, scenario := range specExecutor.specification.Scenarios {
		scenarioResults = append(scenarioResults, specExecutor.getSkippedScenarioResult(scenario))
	}
	specExecutor.specResult.AddScenarioResults(scenarioResults)
	specExecutor.specResult.Skipped = true
	return specExecutor.specResult
}

func (s *specExecutor) getSkippedScenarioResult(scenario *gauge.Scenario) *result.ScenarioResult {
	scenarioResult := &result.ScenarioResult{gauge.NewProtoScenario(scenario)}
	s.addAllItemsForScenarioExecution(scenario, scenarioResult)
	s.setSkipInfoInResult(scenarioResult, scenario)
	return scenarioResult
}

func (specExecutor *specExecutor) execute() *result.SpecResult {
	specInfo := &gauge_messages.SpecInfo{Name: proto.String(specExecutor.specification.Heading.Value),
		FileName: proto.String(specExecutor.specification.FileName),
		IsFailed: proto.Bool(false), Tags: getTagValue(specExecutor.specification.Tags)}
	specExecutor.currentExecutionInfo = &gauge_messages.ExecutionInfo{CurrentSpec: specInfo}
	specExecutor.specResult = gauge.NewSpecResult(specExecutor.specification)
	resolvedSpecItems := specExecutor.resolveItems(specExecutor.specification.GetSpecItems())
	specExecutor.specResult.AddSpecItems(resolvedSpecItems)
	if _, ok := specExecutor.errMap.specErrs[specExecutor.specification]; ok {
		return specExecutor.getSkippedSpecResult()
	}
	specExecutor.consoleReporter.SpecStart(specInfo.GetName())
	beforeSpecHookStatus := specExecutor.executeBeforeSpecHook()
	if beforeSpecHookStatus.GetFailed() {
		result.AddPreHook(specExecutor.specResult, beforeSpecHookStatus)
		setSpecFailure(specExecutor.currentExecutionInfo)
		printStatus(beforeSpecHookStatus, specExecutor.consoleReporter)
	} else {
		dataTableRowCount := specExecutor.specification.DataTable.Table.GetRowCount()
		if dataTableRowCount == 0 {
			scenarioResult := specExecutor.executeScenarios()
			specExecutor.specResult.AddScenarioResults(scenarioResult)
		} else {
			specExecutor.executeTableDrivenSpec()
		}
	}

	afterSpecHookStatus := specExecutor.executeAfterSpecHook()
	if afterSpecHookStatus.GetFailed() {
		result.AddPostHook(specExecutor.specResult, afterSpecHookStatus)
		setSpecFailure(specExecutor.currentExecutionInfo)
		printStatus(afterSpecHookStatus, specExecutor.consoleReporter)
	}
	specExecutor.specResult.Skipped = specExecutor.specResult.ScenarioSkippedCount > 0
	specExecutor.consoleReporter.SpecEnd()
	return specExecutor.specResult
}

func (specExecutor *specExecutor) executeTableDrivenSpec() {
	var dataTableScenarioExecutionResult [][]*result.ScenarioResult
	for specExecutor.currentTableRow = specExecutor.dataTableIndex.start; specExecutor.currentTableRow <= specExecutor.dataTableIndex.end; specExecutor.currentTableRow++ {
		var dataTable gauge.Table
		dataTable.AddHeaders(specExecutor.specification.DataTable.Table.Headers)
		dataTable.AddRowValues(specExecutor.specification.DataTable.Table.Rows()[specExecutor.currentTableRow])
		specExecutor.consoleReporter.DataTable(formatter.FormatTable(&dataTable))
		dataTableScenarioExecutionResult = append(dataTableScenarioExecutionResult, specExecutor.executeScenarios())
	}
	specExecutor.specResult.AddTableDrivenScenarioResult(dataTableScenarioExecutionResult)
}

func getTagValue(tags *gauge.Tags) []string {
	tagValues := make([]string, 0)
	if tags != nil {
		tagValues = append(tagValues, tags.Values...)
	}
	return tagValues
}

func (executor *specExecutor) executeBeforeScenarioHook(scenarioResult *result.ScenarioResult) *gauge_messages.ProtoExecutionResult {
	message := &gauge_messages.Message{MessageType: gauge_messages.Message_ScenarioExecutionStarting.Enum(),
		ScenarioExecutionStartingRequest: &gauge_messages.ScenarioExecutionStartingRequest{CurrentExecutionInfo: executor.currentExecutionInfo}}
	return executor.executeHook(message, scenarioResult)
}

func (executor *specExecutor) initScenarioDataStore() *gauge_messages.ProtoExecutionResult {
	initScenarioDataStoreMessage := &gauge_messages.Message{MessageType: gauge_messages.Message_ScenarioDataStoreInit.Enum(),
		ScenarioDataStoreInitRequest: &gauge_messages.ScenarioDataStoreInitRequest{}}
	initResult := executeAndGetStatus(executor.runner, initScenarioDataStoreMessage)
	if initResult.GetFailed() {
		executor.consoleReporter.Error("Scenario data store didn't get initialized : %s", initResult.ErrorMessage)
	}
	return initResult
}

func (executor *specExecutor) executeAfterScenarioHook(scenarioResult *result.ScenarioResult) *gauge_messages.ProtoExecutionResult {
	message := &gauge_messages.Message{MessageType: gauge_messages.Message_ScenarioExecutionEnding.Enum(),
		ScenarioExecutionEndingRequest: &gauge_messages.ScenarioExecutionEndingRequest{CurrentExecutionInfo: executor.currentExecutionInfo}}
	return executor.executeHook(message, scenarioResult)
}

func (specExecutor *specExecutor) executeScenarios() []*result.ScenarioResult {
	scenarioResults := make([]*result.ScenarioResult, 0)
	for _, scenario := range specExecutor.specification.Scenarios {
		scenarioResults = append(scenarioResults, specExecutor.executeScenario(scenario))
	}
	return scenarioResults
}

func (executor *specExecutor) executeScenario(scenario *gauge.Scenario) *result.ScenarioResult {
	executor.currentExecutionInfo.CurrentScenario = &gauge_messages.ScenarioInfo{Name: proto.String(scenario.Heading.Value), Tags: getTagValue(scenario.Tags), IsFailed: proto.Bool(false)}
	scenarioResult := &result.ScenarioResult{gauge.NewProtoScenario(scenario)}
	executor.addAllItemsForScenarioExecution(scenario, scenarioResult)
	scenarioResult.ProtoScenario.Skipped = proto.Bool(false)
	if _, ok := executor.errMap.scenarioErrs[scenario]; ok {
		executor.setSkipInfoInResult(scenarioResult, scenario)
		return scenarioResult
	}
	executor.consoleReporter.ScenarioStart(scenario.Heading.Value)

	beforeHookExecutionStatus := executor.executeBeforeScenarioHook(scenarioResult)
	if beforeHookExecutionStatus.GetFailed() {
		result.AddPreHook(scenarioResult, beforeHookExecutionStatus)
		setScenarioFailure(executor.currentExecutionInfo)
		printStatus(beforeHookExecutionStatus, executor.consoleReporter)
	} else {
		executor.executeContextItems(scenarioResult)
		if !scenarioResult.GetFailure() {
			executor.executeScenarioItems(scenarioResult)
		}
		executor.executeTearDownItems(scenarioResult)
	}

	afterHookExecutionStatus := executor.executeAfterScenarioHook(scenarioResult)
	result.AddPostHook(scenarioResult, afterHookExecutionStatus)
	scenarioResult.UpdateExecutionTime()
	if afterHookExecutionStatus.GetFailed() {
		setScenarioFailure(executor.currentExecutionInfo)
		printStatus(afterHookExecutionStatus, executor.consoleReporter)
	}
	executor.consoleReporter.ScenarioEnd(scenarioResult.GetFailure())

	return scenarioResult
}

func (executor *specExecutor) setSkipInfoInResult(result *result.ScenarioResult, scenario *gauge.Scenario) {
	executor.specResult.ScenarioSkippedCount += 1
	result.ProtoScenario.Skipped = proto.Bool(true)
	errors := make([]string, 0)
	for _, err := range executor.errMap.scenarioErrs[scenario] {
		errors = append(errors, fmt.Sprintf("%s:%d: %s. %s", err.fileName, err.step.LineNo, err.Error(), err.step.LineText))
	}
	result.ProtoScenario.SkipErrors = errors
}

func (executor *specExecutor) addAllItemsForScenarioExecution(scenario *gauge.Scenario, scenarioResult *result.ScenarioResult) {
	scenarioResult.AddContexts(executor.getContextItemsForScenarioExecution(executor.specification.Contexts))
	scenarioResult.AddTearDownSteps(executor.getContextItemsForScenarioExecution(executor.specification.TearDownSteps))
	scenarioResult.AddItems(executor.resolveItems(scenario.Items))
}

func (executor *specExecutor) getContextItemsForScenarioExecution(steps []*gauge.Step) []*gauge_messages.ProtoItem {
	items := make([]gauge.Item, len(steps))
	for i, context := range steps {
		items[i] = context
	}
	return executor.resolveItems(items)
}

func (executor *specExecutor) executeContextItems(scenarioResult *result.ScenarioResult) {
	failure := executor.executeItems(scenarioResult.ProtoScenario.GetContexts())
	if failure {
		scenarioResult.SetFailure()
	}
}

func (executor *specExecutor) executeTearDownItems(scenarioResult *result.ScenarioResult) {
	failure := executor.executeItems(scenarioResult.ProtoScenario.TearDownSteps)
	if failure {
		scenarioResult.SetFailure()
	}
}

func (executor *specExecutor) executeScenarioItems(scenarioResult *result.ScenarioResult) {
	failure := executor.executeItems(scenarioResult.ProtoScenario.GetScenarioItems())
	if failure {
		scenarioResult.SetFailure()
	}
}

func (executor *specExecutor) resolveItems(items []gauge.Item) []*gauge_messages.ProtoItem {
	protoItems := make([]*gauge_messages.ProtoItem, 0)
	for _, item := range items {
		if item.Kind() != gauge.TearDownKind {
			protoItems = append(protoItems, executor.resolveToProtoItem(item))
		}
	}
	return protoItems
}

func (executor *specExecutor) executeItems(executingItems []*gauge_messages.ProtoItem) bool {
	for _, protoItem := range executingItems {
		failure := executor.executeItem(protoItem)
		if failure == true {
			return true
		}
	}
	return false
}

func (executor *specExecutor) resolveToProtoItem(item gauge.Item) *gauge_messages.ProtoItem {
	var protoItem *gauge_messages.ProtoItem
	switch item.Kind() {
	case gauge.StepKind:
		if (item.(*gauge.Step)).IsConcept {
			concept := item.(*gauge.Step)
			protoItem = executor.resolveToProtoConceptItem(*concept)
		} else {
			protoItem = executor.resolveToProtoStepItem(item.(*gauge.Step))
		}
		break

	default:
		protoItem = gauge.ConvertToProtoItem(item)
	}
	return protoItem
}

func (executor *specExecutor) resolveToProtoStepItem(step *gauge.Step) *gauge_messages.ProtoItem {
	protoStepItem := gauge.ConvertToProtoItem(step)
	paramResolver := new(parser.ParamResolver)
	parameters := paramResolver.GetResolvedParams(step, nil, executor.dataTableLookup())
	updateProtoStepParameters(protoStepItem.Step, parameters)
	executor.setSkipInfo(protoStepItem.Step, step)
	return protoStepItem
}

func (executor *specExecutor) setSkipInfo(protoStep *gauge_messages.ProtoStep, step *gauge.Step) {
	protoStep.StepExecutionResult = &gauge_messages.ProtoStepExecutionResult{}
	protoStep.StepExecutionResult.Skipped = proto.Bool(false)
	if _, ok := executor.errMap.stepErrs[step]; ok {
		protoStep.StepExecutionResult.Skipped = proto.Bool(true)
		protoStep.StepExecutionResult.SkippedReason = proto.String("Step implemenatation not found")
	}
}

// Not passing pointer as we cannot modify the original concept step's lookup. This has to be populated for each iteration over data table.
func (executor *specExecutor) resolveToProtoConceptItem(concept gauge.Step) *gauge_messages.ProtoItem {
	paramResolver := new(parser.ParamResolver)

	parser.PopulateConceptDynamicParams(&concept, executor.dataTableLookup())
	protoConceptItem := gauge.ConvertToProtoItem(&concept)
	protoConceptItem.Concept.ConceptStep.StepExecutionResult = &gauge_messages.ProtoStepExecutionResult{}
	for stepIndex, step := range concept.ConceptSteps {
		// Need to reset parent as the step.parent is pointing to a concept whose lookup is not populated yet
		if step.IsConcept {
			step.Parent = &concept
			protoConceptItem.GetConcept().GetSteps()[stepIndex] = executor.resolveToProtoConceptItem(*step)
		} else {
			stepParameters := paramResolver.GetResolvedParams(step, &concept, executor.dataTableLookup())
			updateProtoStepParameters(protoConceptItem.Concept.Steps[stepIndex].Step, stepParameters)
			executor.setSkipInfo(protoConceptItem.Concept.Steps[stepIndex].Step, step)
		}
	}
	protoConceptItem.Concept.ConceptStep.StepExecutionResult.Skipped = proto.Bool(false)
	return protoConceptItem
}

func updateProtoStepParameters(protoStep *gauge_messages.ProtoStep, parameters []*gauge_messages.Parameter) {
	paramIndex := 0
	for fragmentIndex, fragment := range protoStep.Fragments {
		if fragment.GetFragmentType() == gauge_messages.Fragment_Parameter {
			protoStep.Fragments[fragmentIndex].Parameter = parameters[paramIndex]
			paramIndex++
		}
	}
}

func (executor *specExecutor) dataTableLookup() *gauge.ArgLookup {
	return new(gauge.ArgLookup).FromDataTableRow(&executor.specification.DataTable.Table, executor.currentTableRow)
}

func (executor *specExecutor) executeItem(protoItem *gauge_messages.ProtoItem) bool {
	if protoItem.GetItemType() == gauge_messages.ProtoItem_Concept {
		return executor.executeConcept(protoItem.GetConcept())
	} else if protoItem.GetItemType() == gauge_messages.ProtoItem_Step {
		return executor.executeStep(protoItem.GetStep())
	}
	return false
}

func (executor *specExecutor) executeSteps(protoSteps []*gauge_messages.ProtoStep) bool {
	for _, protoStep := range protoSteps {
		failure := executor.executeStep(protoStep)
		if failure {
			return true
		}
	}
	return false
}

func (executor *specExecutor) executeConcept(protoConcept *gauge_messages.ProtoConcept) bool {
	executor.consoleReporter.ConceptStart(formatter.FormatConcept(protoConcept))
	for _, step := range protoConcept.Steps {
		failure := executor.executeItem(step)
		executor.setExecutionResultForConcept(protoConcept)
		if failure {
			return true
		}
	}
	conceptFailed := protoConcept.GetConceptExecutionResult().GetExecutionResult().GetFailed()
	executor.consoleReporter.ConceptEnd(conceptFailed)
	return conceptFailed
}

func (executor *specExecutor) setExecutionResultForConcept(protoConcept *gauge_messages.ProtoConcept) {
	var conceptExecutionTime int64
	for _, step := range protoConcept.GetSteps() {
		if step.GetItemType() == gauge_messages.ProtoItem_Concept {
			stepExecResult := step.GetConcept().GetConceptExecutionResult().GetExecutionResult()
			conceptExecutionTime += stepExecResult.GetExecutionTime()
			if step.GetConcept().GetConceptExecutionResult().GetExecutionResult().GetFailed() {
				conceptExecutionResult := &gauge_messages.ProtoStepExecutionResult{ExecutionResult: step.GetConcept().GetConceptExecutionResult().GetExecutionResult(), Skipped: proto.Bool(false)}
				conceptExecutionResult.ExecutionResult.ExecutionTime = proto.Int64(conceptExecutionTime)
				protoConcept.ConceptExecutionResult = conceptExecutionResult
				protoConcept.ConceptStep.StepExecutionResult = conceptExecutionResult
				return
			}
		} else if step.GetItemType() == gauge_messages.ProtoItem_Step {
			stepExecResult := step.GetStep().GetStepExecutionResult().GetExecutionResult()
			conceptExecutionTime += stepExecResult.GetExecutionTime()
			if stepExecResult.GetFailed() {
				conceptExecutionResult := &gauge_messages.ProtoStepExecutionResult{ExecutionResult: stepExecResult, Skipped: proto.Bool(false)}
				conceptExecutionResult.ExecutionResult.ExecutionTime = proto.Int64(conceptExecutionTime)
				protoConcept.ConceptExecutionResult = conceptExecutionResult
				protoConcept.ConceptStep.StepExecutionResult = conceptExecutionResult
				return
			}
		}
	}
	protoConcept.ConceptExecutionResult = &gauge_messages.ProtoStepExecutionResult{ExecutionResult: &gauge_messages.ProtoExecutionResult{Failed: proto.Bool(false), ExecutionTime: proto.Int64(conceptExecutionTime)}}
	protoConcept.ConceptStep.StepExecutionResult = protoConcept.ConceptExecutionResult
	protoConcept.ConceptStep.StepExecutionResult.Skipped = proto.Bool(false)
}

func printStatus(executionResult *gauge_messages.ProtoExecutionResult, reporter reporter.Reporter) {
	reporter.Error("Error Message: %s", executionResult.GetErrorMessage())
	reporter.Error("Stacktrace: \n%s", executionResult.GetStackTrace())
}

func (executor *specExecutor) executeStep(protoStep *gauge_messages.ProtoStep) bool {
	stepRequest := executor.createStepRequest(protoStep)
	stepText := formatter.FormatStep(parser.CreateStepFromStepRequest(stepRequest))
	executor.consoleReporter.StepStart(stepText)

	protoStepExecResult := &gauge_messages.ProtoStepExecutionResult{}
	executor.currentExecutionInfo.CurrentStep = &gauge_messages.StepInfo{Step: stepRequest, IsFailed: proto.Bool(false)}

	beforeHookStatus := executor.executeBeforeStepHook()
	if beforeHookStatus.GetFailed() {
		protoStepExecResult.PreHookFailure = result.GetProtoHookFailure(beforeHookStatus)
		protoStepExecResult.ExecutionResult = &gauge_messages.ProtoExecutionResult{Failed: proto.Bool(true)}
		setStepFailure(executor.currentExecutionInfo, executor.consoleReporter)
		printStatus(beforeHookStatus, executor.consoleReporter)
	} else {
		executeStepMessage := &gauge_messages.Message{MessageType: gauge_messages.Message_ExecuteStep.Enum(), ExecuteStepRequest: stepRequest}
		stepExecutionStatus := executeAndGetStatus(executor.runner, executeStepMessage)
		if stepExecutionStatus.GetFailed() {
			setStepFailure(executor.currentExecutionInfo, executor.consoleReporter)
		}
		protoStepExecResult.ExecutionResult = stepExecutionStatus
	}
	afterStepHookStatus := executor.executeAfterStepHook()
	addExecutionTimes(protoStepExecResult, beforeHookStatus, afterStepHookStatus)
	if afterStepHookStatus.GetFailed() {
		setStepFailure(executor.currentExecutionInfo, executor.consoleReporter)
		printStatus(afterStepHookStatus, executor.consoleReporter)
		protoStepExecResult.PostHookFailure = result.GetProtoHookFailure(afterStepHookStatus)
		protoStepExecResult.ExecutionResult.Failed = proto.Bool(true)
	}
	protoStepExecResult.ExecutionResult.Message = afterStepHookStatus.Message
	protoStepExecResult.Skipped = protoStep.StepExecutionResult.Skipped
	protoStepExecResult.SkippedReason = protoStep.StepExecutionResult.SkippedReason
	protoStep.StepExecutionResult = protoStepExecResult

	stepFailed := protoStep.GetStepExecutionResult().GetExecutionResult().GetFailed()
	executor.consoleReporter.StepEnd(stepFailed)
	if stepFailed {
		result := protoStep.GetStepExecutionResult().GetExecutionResult()
		executor.consoleReporter.Error("Failed Step: %s", executor.currentExecutionInfo.CurrentStep.Step.GetActualStepText())
		executor.consoleReporter.Error("Error Message: %s", strings.TrimSpace(result.GetErrorMessage()))
		executor.consoleReporter.Error("Stacktrace: \n%s", result.GetStackTrace())
	}
	return stepFailed
}

func addExecutionTimes(stepExecResult *gauge_messages.ProtoStepExecutionResult, execResults ...*gauge_messages.ProtoExecutionResult) {
	for _, execResult := range execResults {
		currentScenarioExecTime := stepExecResult.ExecutionResult.ExecutionTime
		if currentScenarioExecTime == nil {
			stepExecResult.ExecutionResult.ExecutionTime = proto.Int64(execResult.GetExecutionTime())
		} else {
			stepExecResult.ExecutionResult.ExecutionTime = proto.Int64(*currentScenarioExecTime + execResult.GetExecutionTime())
		}
	}
}

func (executor *specExecutor) executeBeforeStepHook() *gauge_messages.ProtoExecutionResult {
	message := &gauge_messages.Message{MessageType: gauge_messages.Message_StepExecutionStarting.Enum(),
		StepExecutionStartingRequest: &gauge_messages.StepExecutionStartingRequest{CurrentExecutionInfo: executor.currentExecutionInfo}}
	executor.pluginHandler.NotifyPlugins(message)
	return executeAndGetStatus(executor.runner, message)
}

func (executor *specExecutor) executeAfterStepHook() *gauge_messages.ProtoExecutionResult {
	message := &gauge_messages.Message{MessageType: gauge_messages.Message_StepExecutionEnding.Enum(),
		StepExecutionEndingRequest: &gauge_messages.StepExecutionEndingRequest{CurrentExecutionInfo: executor.currentExecutionInfo}}
	executor.pluginHandler.NotifyPlugins(message)
	return executeAndGetStatus(executor.runner, message)
}

func (executor *specExecutor) createStepRequest(protoStep *gauge_messages.ProtoStep) *gauge_messages.ExecuteStepRequest {
	stepRequest := &gauge_messages.ExecuteStepRequest{ParsedStepText: proto.String(protoStep.GetParsedText()), ActualStepText: proto.String(protoStep.GetActualText())}
	stepRequest.Parameters = getParameters(protoStep.GetFragments())
	return stepRequest
}

func (executor *specExecutor) getCurrentDataTableValueFor(columnName string) string {
	return executor.specification.DataTable.Table.Get(columnName)[executor.currentTableRow].Value
}

func executeAndGetStatus(runner *runner.TestRunner, message *gauge_messages.Message) *gauge_messages.ProtoExecutionResult {
	response, err := conn.GetResponseForGaugeMessage(message, runner.Connection)
	if err != nil {
		return &gauge_messages.ProtoExecutionResult{Failed: proto.Bool(true), ErrorMessage: proto.String(err.Error())}
	}

	if response.GetMessageType() == gauge_messages.Message_ExecutionStatusResponse {
		executionResult := response.GetExecutionStatusResponse().GetExecutionResult()
		if executionResult == nil {
			errMsg := "ProtoExecutionResult obtained is nil"
			logger.Error(errMsg)
			return errorResult(errMsg)
		}
		return executionResult
	} else {
		errMsg := fmt.Sprintf("Expected ExecutionStatusResponse. Obtained: %s", response.GetMessageType())
		logger.Error(errMsg)
		return errorResult(errMsg)
	}
}

func errorResult(message string) *gauge_messages.ProtoExecutionResult {
	return &gauge_messages.ProtoExecutionResult{Failed: proto.Bool(true), ErrorMessage: proto.String(message), RecoverableError: proto.Bool(false)}
}

func setSpecFailure(executionInfo *gauge_messages.ExecutionInfo) {
	executionInfo.CurrentSpec.IsFailed = proto.Bool(true)
}

func setScenarioFailure(executionInfo *gauge_messages.ExecutionInfo) {
	setSpecFailure(executionInfo)
	executionInfo.CurrentScenario.IsFailed = proto.Bool(true)
}

func setStepFailure(executionInfo *gauge_messages.ExecutionInfo, reporter reporter.Reporter) {
	setScenarioFailure(executionInfo)
	executionInfo.CurrentStep.IsFailed = proto.Bool(true)
}

func getParameters(fragments []*gauge_messages.Fragment) []*gauge_messages.Parameter {
	parameters := make([]*gauge_messages.Parameter, 0)
	for _, fragment := range fragments {
		if fragment.GetFragmentType() == gauge_messages.Fragment_Parameter {
			parameters = append(parameters, fragment.GetParameter())
		}
	}
	return parameters
}

func getDataTableRowsRange(tableRows string, rowCount int) (indexRange, error) {
	var startIndex, endIndex int
	var err error
	indexRanges := strings.Split(tableRows, "-")
	if len(indexRanges) == 2 {
		startIndex, endIndex, err = validateTableRowsRange(indexRanges[0], indexRanges[1], rowCount)
	} else if len(indexRanges) == 1 {
		startIndex, endIndex, err = validateTableRowsRange(tableRows, tableRows, rowCount)
	} else {
		return indexRange{start: 0, end: 0}, errors.New("Table rows range validation failed.")
	}
	if err != nil {
		return indexRange{start: 0, end: 0}, err
	}
	return indexRange{start: startIndex, end: endIndex}, nil
}
