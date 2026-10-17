/**
 * Wails v3 Compatibility Shim
 * 
 * This file provides backward compatibility for Wails v2 style calls (window.go.main.App.*)
 * by mapping them to Wails v3 Call API.
 */

import { Call } from '@wailsio/runtime'

// Wails v3 service name format: module/package.struct
// Format: openai-router-go/services.AppService
const SERVICE = 'openai-router-go/services.AppService'


// Helper function to call service methods
// Wails v3 Call.ByName expects method name and arguments as separate parameters
const callService = async (method, ...args) => {
  const fullMethod = `${SERVICE}.${method}`
  console.log(`Calling: ${fullMethod}`, args)
  try {
    // Try with spread args first
    const result = await Call.ByName(fullMethod, ...args)
    console.log(`Result from ${fullMethod}:`, result)
    return result
  } catch (error) {
    console.error(`Error calling ${fullMethod}:`, error)
    throw error
  }
}

// Create the window.go.main.App object for backward compatibility
const createWailsShim = () => {
  const App = {
    // Route management
    GetRoutes: () => callService('GetRoutes'),
    AddRoute: (name, model, apiUrl, apiKey, format) =>
      callService('AddRoute', name, model, apiUrl, apiKey, format),
    UpdateRoute: (id, name, model, apiUrl, apiKey, format) =>
      callService('UpdateRoute', id, name, model, apiUrl, apiKey, format),
    DeleteRoute: (id) => callService('DeleteRoute', id),

    // Failover groups
    GetFailoverGroups: () => callService('GetFailoverGroups'),
    AddFailoverGroup: (name, model, cooldownSeconds) =>
      callService('AddFailoverGroup', name, model, cooldownSeconds),
    UpdateFailoverGroup: (id, name, model, cooldownSeconds, enabled) =>
      callService('UpdateFailoverGroup', id, name, model, cooldownSeconds, enabled),
    DeleteFailoverGroup: (id) => callService('DeleteFailoverGroup', id),
    AddFailoverEndpoint: (groupID, name, apiUrl, apiKey, format) =>
      callService('AddFailoverEndpoint', groupID, name, apiUrl, apiKey, format),
    UpdateFailoverEndpoint: (id, name, apiUrl, apiKey, format, priority) =>
      callService('UpdateFailoverEndpoint', id, name, apiUrl, apiKey, format, priority),
    DeleteFailoverEndpoint: (id) => callService('DeleteFailoverEndpoint', id),
    MoveFailoverEndpoint: (id, direction) => callService('MoveFailoverEndpoint', id, direction),
    
    // Statistics
    GetStats: () => callService('GetStats'),
    GetDailyStats: (days) => callService('GetDailyStats', days),
    GetHourlyStats: () => callService('GetHourlyStats'),
    GetSecondlyStats: (minutes) => callService('GetSecondlyStats', minutes),
    GetModelRanking: (limit) => callService('GetModelRanking', limit),
    ClearStats: () => callService('ClearStats'),
    
    // Configuration
    GetConfig: () => callService('GetConfig'),
    UpdateConfig: (redirectEnabled, redirectKeyword, redirectTargetModel, redirectTargetRouteId, redirectTargetGroupId) =>
      callService('UpdateConfig', redirectEnabled, redirectKeyword, redirectTargetModel, redirectTargetRouteId, redirectTargetGroupId),
    UpdatePort: (port) => callService('UpdatePort', port),
    UpdateLocalApiKey: (newApiKey) => callService('UpdateLocalApiKey', newApiKey),
    RestartApp: () => callService('RestartApp'),
    
    // App settings
    GetAppSettings: () => callService('GetAppSettings'),
    SetMinimizeToTray: (enabled) => callService('SetMinimizeToTray', enabled),
    SetAutoStart: (enabled) => callService('SetAutoStart', enabled),
    SetEnableFileLog: (enabled) => callService('SetEnableFileLog', enabled),
    
    // Remote models
    FetchRemoteModels: (apiUrl, apiKey) => callService('FetchRemoteModels', apiUrl, apiKey),
    
    // Import
    ImportRouteFromFormat: (name, model, apiUrl, apiKey, targetFormat) =>
      callService('ImportRouteFromFormat', name, model, apiUrl, apiKey, targetFormat),
    
    // Database management
    CompressDatabase: () => callService('CompressDatabase'),
    GetUsageSummary: () => callService('GetUsageSummary'),
  }

  // Create the window.go.main.App structure
  window.go = {
    main: {
      App: App
    }
  }
  
  console.log('Wails v3 shim initialized')
}

// Initialize the shim
createWailsShim()

export default createWailsShim
