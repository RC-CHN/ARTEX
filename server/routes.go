package server

import "net/http"

// Handler assembles the API by domain. Authentication and CORS wrap the entire
// API mux; the embedded frontend remains on the separate public root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerSideRoutes(mux)
	s.registerAuthRoutes(mux)
	s.registerSystemRoutes(mux)
	s.registerTaskRoutes(mux)
	s.registerAssetRoutes(mux)
	s.registerFindingRoutes(mux)
	s.registerExplorationRoutes(mux)
	s.registerObservabilityRoutes(mux)
	s.registerLLMRoutes(mux)
	s.registerTrafficRoutes(mux)
	s.registerSettingsRoutes(mux)
	s.registerNotificationRoutes(mux)
	s.registerChatRoutes(mux)
	s.registerAgentRoutes(mux)
	s.registerResourceRoutes(mux)
	s.registerWorkspaceRoutes(mux)
	s.registerInterceptRoutes(mux)
	mux.HandleFunc("GET /api/runners", s.getRunners)
	mux.HandleFunc("PUT /api/runners/config", s.saveRunnerConfig)
	mux.HandleFunc("POST /api/runners/registration-token", s.runnerRegistration)
	mux.HandleFunc("PUT /api/runners/{id}/enabled", s.setRunnerEnabled)
	mux.HandleFunc("PUT /api/runners/{id}/concurrency", s.setRunnerConcurrency)
	mux.HandleFunc("PUT /api/runners/{id}/limits", s.setRunnerLimits)
	mux.HandleFunc("PUT /api/runners/{id}/environment-prompt", s.setRunnerEnvironmentPrompt)

	api := cors(s.requireAuth(mux))
	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/runner/v1/", s.runnerProtocolHandler())
	root.Handle("/", s.webuiHandler())
	return root
}

// Public authentication endpoints are selected by requireAuth; changing a
// registration here does not bypass that middleware.
func (s *Server) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/init", s.authInit)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/change-password", s.authChangePassword)
}

// Self-update and audit routes remain protected by the common API middleware.
func (s *Server) registerSystemRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/update/check", s.updateCheck)
	mux.HandleFunc("POST /api/update/apply", s.updateApply)
	mux.HandleFunc("POST /api/update/rollback", s.updateRollback)
	mux.HandleFunc("GET /api/update/stream", s.updateStream)
	mux.HandleFunc("GET /api/audit", s.getAudit)
	mux.HandleFunc("POST /api/gc", s.gc)
}

// Task lifecycle, scope, controls, and archives share the same task boundary.
func (s *Server) registerTaskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("GET /api/task-categories", s.pgListTaskCategories)
	mux.HandleFunc("POST /api/task-categories", s.pgCreateTaskCategory)
	mux.HandleFunc("PATCH /api/task-categories/{id}", s.pgRenameTaskCategory)
	mux.HandleFunc("DELETE /api/task-categories/{id}", s.pgDeleteTaskCategory)
	mux.HandleFunc("POST /api/tasks/category/batch", s.updateTasksCategoryBatch)
	mux.HandleFunc("GET /api/task-templates", s.pgListTaskTemplates)
	mux.HandleFunc("POST /api/task-templates", s.pgCreateTaskTemplate)
	mux.HandleFunc("PATCH /api/task-templates/{id}", s.pgUpdateTaskTemplate)
	mux.HandleFunc("DELETE /api/task-templates/{id}", s.pgDeleteTaskTemplate)
	mux.HandleFunc("GET /api/tasks/{id}", s.getTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.updateTaskMetadata)
	mux.HandleFunc("PATCH /api/tasks/{id}/category", s.updateTaskCategory)
	mux.HandleFunc("GET /api/tasks/{id}/intercept-rules", s.taskInterceptListRules)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules", s.taskInterceptCreateRule)
	mux.HandleFunc("PUT /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/tasks/{id}/intercept-rules/{rid}", s.taskInterceptDeleteRule)
	mux.HandleFunc("POST /api/tasks/{id}/intercept-rules/{rid}/toggle", s.taskInterceptToggleRule)
	mux.HandleFunc("POST /api/tasks/control/batch", s.controlTasksBatch)
	mux.HandleFunc("GET /api/task-archives", s.listTaskArchives)
	mux.HandleFunc("GET /api/task-archives/{id}", s.getTaskArchive)
	mux.HandleFunc("POST /api/tasks/{id}/archive", s.queueTaskArchive)
	mux.HandleFunc("POST /api/tasks/archive/batch", s.queueTaskArchivesBatch)
	mux.HandleFunc("POST /api/task-archives/{id}/restore", s.queueTaskArchiveRestore)
	mux.HandleFunc("POST /api/task-archives/restore/batch", s.restoreTaskArchivesBatch)
	mux.HandleFunc("DELETE /api/task-archives/{id}", s.queueTaskArchiveDelete)
	mux.HandleFunc("POST /api/task-archives/delete/batch", s.deleteTaskArchivesBatch)
	mux.HandleFunc("GET /api/tasks/{id}/coverage", s.taskCoverage)
	mux.HandleFunc("GET /api/tasks/{id}/coverage-graph", s.taskCoverageGraph)
	mux.HandleFunc("GET /api/tasks/{id}/asset-refs", s.taskAssetRefs)
	mux.HandleFunc("POST /api/tasks/{id}/assets", s.attachTaskAssets)
	mux.HandleFunc("DELETE /api/tasks/{id}/assets/{assetID}", s.detachTaskAsset)
	mux.HandleFunc("GET /api/tasks/{id}/intent-assets", s.taskIntentAssets)
	mux.HandleFunc("GET /api/tasks/{id}/scope", s.taskScopeList)
	mux.HandleFunc("POST /api/tasks/{id}/scope", s.taskScopeAdd)
	mux.HandleFunc("DELETE /api/tasks/{id}/scope/{sid}", s.taskScopeDelete)
	mux.HandleFunc("GET /api/tasks/{id}/goals", s.listGoals)                       // 目标管理:列出本任务全部目标
	mux.HandleFunc("POST /api/tasks/{id}/goals", s.addGoal)                        // 目标管理:人工新增目标(复活任务)
	mux.HandleFunc("PATCH /api/tasks/{id}/goals/{gid}", s.editGoal)                // 目标管理:修改目标(复活任务)
	mux.HandleFunc("DELETE /api/tasks/{id}/goals/{gid}", s.deleteGoal)             // 目标管理:硬删除目标(不复活)
	mux.HandleFunc("GET /api/tasks/{id}/constraints", s.listConstraints)           // 约束管理:列出本任务操作约束
	mux.HandleFunc("POST /api/tasks/{id}/constraints", s.addConstraint)            // 约束管理:新增约束(不通知 planner)
	mux.HandleFunc("PATCH /api/tasks/{id}/constraints/{cid}", s.editConstraint)    // 约束管理:修改约束
	mux.HandleFunc("DELETE /api/tasks/{id}/constraints/{cid}", s.deleteConstraint) // 约束管理:删除约束
	mux.HandleFunc("POST /api/tasks/{id}/control", s.control)
	mux.HandleFunc("PUT /api/tasks/{id}/llm", s.updateTaskLLMProfiles)
	mux.HandleFunc("GET /api/tasks/{id}/llm/resolution", s.taskLLMResolutionHandler)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/control", s.controlIntent)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/messages", s.sendWorkerMessage)
	mux.HandleFunc("POST /api/tasks/{id}/intents/{iid}/rerun", s.rerunIntent)    // 重跑单条 blocked/exhausted/stopped 意图
	mux.HandleFunc("POST /api/tasks/{id}/intents/rerun-blocked", s.rerunBlocked) // 批量重跑本任务全部 blocked 意图
	mux.HandleFunc("POST /api/active", s.setActive)
	mux.HandleFunc("GET /api/report", s.getReport)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.pgDeleteTask)
}

func (s *Server) registerAssetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/assets", s.listAssets)
	mux.HandleFunc("GET /api/assets/counts", s.assetCounts)
	mux.HandleFunc("POST /api/assets", s.insertAssets)
	mux.HandleFunc("DELETE /api/assets", s.deleteAssets)
	mux.HandleFunc("GET /api/companies", s.listCompanies)
	mux.HandleFunc("POST /api/companies", s.createCompany)
	mux.HandleFunc("GET /api/companies/{id}", s.getCompany)
	mux.HandleFunc("DELETE /api/companies/{id}", s.deleteCompany)
	mux.HandleFunc("POST /api/companies/{id}/scope", s.addCompanyScope)
	mux.HandleFunc("POST /api/companies/reattribute", s.reattribute)
	mux.HandleFunc("GET /api/sync/scopesentry/status", s.syncSSStatus)
	mux.HandleFunc("POST /api/sync/scopesentry/datasource", s.syncSSDatasource)
	mux.HandleFunc("GET /api/sync/scopesentry/projects", s.syncSSProjects)
	mux.HandleFunc("GET /api/sync/scopesentry/tasks", s.syncSSTasks)
	mux.HandleFunc("POST /api/sync/scopesentry/sync", s.syncSSRun)
}

func (s *Server) registerFindingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/exploration/findings", s.findings)
	mux.HandleFunc("GET /api/exploration/findings/groups", s.findingGroups)
	mux.HandleFunc("GET /api/exploration/findings/asset-tree", s.findingAssetTree)
	mux.HandleFunc("GET /api/exploration/findings/stats", s.findingStats)
	mux.HandleFunc("GET /api/exploration/findings/export", s.findingsExport)
	s.registerFindingTraffic(mux)
	mux.HandleFunc("GET /api/exploration/findings/{id}", s.getFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/lineage", s.findingLineage)
	mux.HandleFunc("POST /api/exploration/findings/{id}/deepen", s.deepenFinding)
	mux.HandleFunc("GET /api/exploration/findings/{id}/retests", s.listFindingRetests)
	mux.HandleFunc("GET /api/exploration/findings/retests/active", s.listActiveFindingRetests)
	mux.HandleFunc("POST /api/exploration/findings/{id}/retests", s.startFindingRetest)
	mux.HandleFunc("PATCH /api/exploration/findings/{id}", s.patchFinding)
	mux.HandleFunc("DELETE /api/exploration/findings/{id}", s.deleteFinding)
}

func (s *Server) registerExplorationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/exploration/frontier", s.frontier)
	mux.HandleFunc("GET /api/exploration/intents", s.intents)
	mux.HandleFunc("GET /api/exploration/graph", s.explorationGraph)
	mux.HandleFunc("GET /api/exploration/nodes", s.explorationNodes)
}

func (s *Server) registerObservabilityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/logs", s.getLogs)
	mux.HandleFunc("GET /api/logs/history", s.getLogsHistory)
	mux.HandleFunc("GET /api/logs/stream", s.streamLogs)
	mux.HandleFunc("GET /api/exploration/activity", s.activity)
	mux.HandleFunc("GET /api/exploration/activity/history", s.activityHistory)
	mux.HandleFunc("GET /api/exploration/activity/stream", s.streamActivity)
	mux.HandleFunc("GET /api/exploration/activity/{seq}", s.activityDetail)
	mux.HandleFunc("GET /api/exploration/tokens", s.tokenStats)
	mux.HandleFunc("GET /api/tokens/daily", s.tokenDailyStats)
	mux.HandleFunc("GET /api/tokens/conversations", s.conversationTokens)
	mux.HandleFunc("GET /api/tokens/usage", s.pgUsageStats) // 全局 llm_usage 聚合（仪表盘新版视图）
	mux.HandleFunc("GET /api/commands", s.pgListCommands)
	mux.HandleFunc("GET /api/commands/stats", s.pgToolStats) // 按工具聚合调用次数
}

func (s *Server) registerLLMRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/llm", s.getLLM)
	mux.HandleFunc("POST /api/llm", s.setLLM)
	mux.HandleFunc("POST /api/llm/test", s.testLLM)
	mux.HandleFunc("GET /api/llm/records", s.pgListLLMRecords)
	mux.HandleFunc("DELETE /api/llm/records", s.pgDeleteLLMRecords)
	mux.HandleFunc("GET /api/llm/records/tasks", s.pgLLMTasks)
	mux.HandleFunc("GET /api/llm/records/by-model", s.pgTokenByModel) // 按模型聚合本任务 token 用量
	mux.HandleFunc("GET /api/llm/records/{id}", s.pgGetLLMRecord)
	mux.HandleFunc("GET /api/llm/profiles", s.pgListProfiles)
	mux.HandleFunc("POST /api/llm/profiles", s.pgSaveProfile)
	mux.HandleFunc("DELETE /api/llm/profiles/{id}", s.pgDeleteProfile)
	mux.HandleFunc("POST /api/llm/profiles/active", s.pgActivateProfile)
	mux.HandleFunc("GET /api/llm/retry-policy", s.pgGetLLMRetryPolicy)
	mux.HandleFunc("POST /api/llm/retry-policy", s.pgSaveLLMRetryPolicy)
	mux.HandleFunc("GET /api/llm/pool", s.pgLLMPoolStatus)
	mux.HandleFunc("POST /api/llm/pool/reset", s.pgLLMPoolReset)
	mux.HandleFunc("POST /api/llm/models", s.pgListModels)
}

func (s *Server) registerTrafficRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/traffic", s.getTraffic)
	mux.HandleFunc("GET /api/traffic/hosts", s.getTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic", s.deleteTraffic)
	mux.HandleFunc("DELETE /api/traffic/hosts", s.deleteTrafficHosts)
	mux.HandleFunc("DELETE /api/traffic/all", s.deleteAllTraffic)
	mux.HandleFunc("GET /api/traffic/exchange", s.getTrafficExchange)
	mux.HandleFunc("GET /api/traffic/blob", s.getTrafficBlob)
}

func (s *Server) registerSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/settings/web-search/test", s.testWebSearch)
	mux.HandleFunc("POST /api/settings/python/detect", s.pgDetectPython)
}

func (s *Server) registerNotificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/notify/meta", s.notifyMeta)
	mux.HandleFunc("GET /api/notify/channels", s.notifyListChannels)
	mux.HandleFunc("POST /api/notify/channels", s.notifyCreateChannel)
	mux.HandleFunc("PATCH /api/notify/channels/{id}", s.notifyUpdateChannel)
	mux.HandleFunc("DELETE /api/notify/channels/{id}", s.notifyDeleteChannel)
	mux.HandleFunc("POST /api/notify/channels/{id}/test", s.notifyTestChannel)
	mux.HandleFunc("GET /api/notify/deliveries", s.notifyListDeliveries)
	mux.HandleFunc("POST /api/notify/deliveries/{id}/retry", s.notifyRetryDelivery)
}

func (s *Server) registerChatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/exploration/main-sessions", s.mainSessions)
	mux.HandleFunc("POST /api/exploration/main-session/new", s.newMainSession)
	mux.HandleFunc("GET /api/chat/mentions", s.searchChatMentions)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/chat/upload", s.chatUpload) // 方式1 文件上传:落到会话/任务工作目录 uploads/
	mux.HandleFunc("GET /api/tasks/{id}/chat/status", s.taskChatStatus)
	mux.HandleFunc("POST /api/tasks/{id}/chat/stop", s.stopChat)
	mux.HandleFunc("GET /api/conversations", s.pgListConversations)
	mux.HandleFunc("POST /api/conversations", s.pgCreateConversation)
	mux.HandleFunc("POST /api/conversations/delete/batch", s.pgDeleteConversationsBatch)
	mux.HandleFunc("PATCH /api/conversations/{id}", s.pgRenameConversation)
	mux.HandleFunc("PATCH /api/conversations/{id}/profile", s.pgUpdateConversation)
	mux.HandleFunc("DELETE /api/conversations/{id}", s.pgDeleteConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.pgConversationMessages)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.pgSendConversationMessage)
	mux.HandleFunc("POST /api/conversations/{id}/stop", s.pgStopConversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages/{seq}", s.pgConversationMsgDetail)
}

func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", s.pgListAgents)
	mux.HandleFunc("POST /api/agents", s.pgCreateAgent)
	mux.HandleFunc("GET /api/agents/{key}", s.pgGetAgent)
	mux.HandleFunc("PATCH /api/agents/{key}", s.pgUpdateAgent)
	mux.HandleFunc("DELETE /api/agents/{key}", s.pgDeleteAgent)
	mux.HandleFunc("PUT /api/agents/{key}/config", s.pgSaveAgentConfig)
	mux.HandleFunc("PUT /api/agents/{key}/prompt", s.pgSavePrompt)
	mux.HandleFunc("POST /api/agents/{key}/prompt/reset", s.pgResetPrompt)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup", s.pgSaveWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/reset", s.pgResetWrapup)
	mux.HandleFunc("PUT /api/agents/{key}/wrapup/task-timeout", s.pgSaveTaskTimeoutWrapup)
	mux.HandleFunc("POST /api/agents/{key}/wrapup/task-timeout/reset", s.pgResetTaskTimeoutWrapup)
	mux.HandleFunc("GET /api/agents/{key}/triggers", s.pgListTriggers)
	mux.HandleFunc("POST /api/agents/{key}/triggers", s.pgCreateTrigger)
	mux.HandleFunc("PATCH /api/triggers/{id}", s.pgUpdateTrigger)
	mux.HandleFunc("DELETE /api/triggers/{id}", s.pgDeleteTrigger)
	mux.HandleFunc("GET /api/agents/{key}/prompts", s.pgListPromptVersions)
	mux.HandleFunc("GET /api/agents/{key}/variables", s.pgPromptVars)
	mux.HandleFunc("POST /api/agents/{key}/prompt/preview", s.pgPreviewPrompt)
	mux.HandleFunc("GET /api/agents/{key}/visibility", s.pgGetAgentVisibility)
	mux.HandleFunc("PUT /api/agents/{key}/visibility", s.pgSetAgentVisibility)
}

// Resource routes configure the tools, MCP servers, skills, and visibility
// available to agents. ServeMux resolves specific skill patterns over wildcards.
func (s *Server) registerResourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools", s.pgListTools)
	mux.HandleFunc("PUT /api/tools/{key}", s.pgUpdateTool)
	mux.HandleFunc("POST /api/tools/custom", s.pgCreateCustomTool)
	mux.HandleFunc("POST /api/tools/custom/test", s.pgTestCustomTool)
	mux.HandleFunc("PUT /api/tools/custom/{key}", s.pgUpdateCustomTool)
	mux.HandleFunc("DELETE /api/tools/custom/{key}", s.pgDeleteCustomTool)
	mux.HandleFunc("POST /api/tools/{key}/reset", s.pgResetTool)
	mux.HandleFunc("GET /api/mcp", s.pgListMCP)
	mux.HandleFunc("POST /api/mcp", s.pgSaveMCP)
	mux.HandleFunc("DELETE /api/mcp/{id}", s.pgDeleteMCP)
	mux.HandleFunc("GET /api/mcp/{id}/tools", s.pgMCPTools)
	mux.HandleFunc("POST /api/mcp/{id}/refresh", s.pgRefreshMCP)
	mux.HandleFunc("GET /api/skills", s.fsListSkills)
	mux.HandleFunc("POST /api/skills", s.fsCreateSkill)
	mux.HandleFunc("POST /api/skills/upload", s.fsUploadSkill)
	mux.HandleFunc("DELETE /api/skills/{name}", s.fsDeleteSkill)
	mux.HandleFunc("GET /api/skills/missing", s.fsMissingSkills)   // 未命中(想调但不存在)的 skill 名
	mux.HandleFunc("GET /api/skills/{name}/usage", s.fsSkillUsage) // 单个 skill 的最近调用
	mux.HandleFunc("PUT /api/skills/{name}/meta", s.fsUpdateSkillMeta)
	mux.HandleFunc("POST /api/skills/{name}/dirs", s.fsCreateDir)
	mux.HandleFunc("GET /api/skills/{name}/files", s.fsListFiles)
	mux.HandleFunc("GET /api/skills/{name}/files/{file...}", s.fsReadFile)
	mux.HandleFunc("PUT /api/skills/{name}/files/{file...}", s.fsWriteFile)
	mux.HandleFunc("DELETE /api/skills/{name}/files/{file...}", s.fsDeletePath)
	mux.HandleFunc("GET /api/visibility/{kind}/{id}", s.pgResourceVisibility)
	mux.HandleFunc("POST /api/visibility/toggle", s.pgToggleVisibility)
	mux.HandleFunc("GET /api/visibility/skill/{name}", s.pgSkillVisibility)
	mux.HandleFunc("POST /api/visibility/skill/toggle", s.pgToggleSkillVisibility)
}

func (s *Server) registerWorkspaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workspace/list", s.wsList)
	mux.HandleFunc("GET /api/workspace/read", s.wsRead)
	mux.HandleFunc("POST /api/workspace/write", s.wsWrite)
	mux.HandleFunc("POST /api/workspace/mkdir", s.wsMkdir)
	mux.HandleFunc("DELETE /api/workspace/delete", s.wsDelete)
	mux.HandleFunc("GET /api/workspace/download", s.wsDownload)
	mux.HandleFunc("POST /api/workspace/upload", s.wsUpload)
}

func (s *Server) registerInterceptRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/intercept/rules", s.interceptListRules)
	mux.HandleFunc("POST /api/intercept/rules", s.interceptCreateRule)
	mux.HandleFunc("PUT /api/intercept/rules/{id}", s.interceptUpdateRule)
	mux.HandleFunc("DELETE /api/intercept/rules/{id}", s.interceptDeleteRule)
	mux.HandleFunc("POST /api/intercept/rules/{id}/toggle", s.interceptToggleRule)
	mux.HandleFunc("GET /api/asset-intercept/rules", s.assetInterceptListRules)
	mux.HandleFunc("POST /api/asset-intercept/rules", s.assetInterceptCreateRule)
	mux.HandleFunc("PUT /api/asset-intercept/rules/{id}", s.assetInterceptUpdateRule)
	mux.HandleFunc("DELETE /api/asset-intercept/rules/{id}", s.assetInterceptDeleteRule)
	mux.HandleFunc("POST /api/asset-intercept/rules/{id}/toggle", s.assetInterceptToggleRule)
	mux.HandleFunc("GET /api/intercept/pending", s.interceptListPending)
	mux.HandleFunc("GET /api/intercept/pending/{id}", s.interceptGetOne)
	mux.HandleFunc("POST /api/intercept/pending/{id}/decide", s.interceptDecide)
	mux.HandleFunc("GET /api/intercept/history", s.interceptHistory)
	mux.HandleFunc("GET /api/intercept/history/{id}", s.interceptDetail)
	mux.HandleFunc("GET /api/intercept/history/{id}/execution", s.interceptExecution)
	mux.HandleFunc("GET /api/intercept/task/{taskID}", s.interceptListTaskItems)
	mux.HandleFunc("GET /api/intercept/tool-config", s.interceptGetToolConfig)
	mux.HandleFunc("PUT /api/intercept/tool-config", s.interceptSetToolConfig)
	mux.HandleFunc("GET /api/intercept/judge", s.interceptGetJudgeConfig)
	mux.HandleFunc("PUT /api/intercept/judge", s.interceptSetJudgeConfig)
	mux.HandleFunc("GET /api/intercept/judge/usage", s.interceptJudgeUsage) // 兜底审批累计 token 用量
}
