package audit

import "strconv"

// recordTypes maps the numeric RecordType found on every Office 365 audit
// record to its symbolic name, as published in the Office 365 Management
// Activity API schema. The list is a subset covering the record types that
// occur in practice; unknown values fall back to "RecordType_<n>".
var recordTypes = map[int]string{
	1:   "ExchangeAdmin",
	2:   "ExchangeItem",
	3:   "ExchangeItemGroup",
	4:   "SharePoint",
	5:   "SyncEvent",
	6:   "SharePointFileOperation",
	7:   "OneDrive",
	8:   "AzureActiveDirectory",
	9:   "AzureActiveDirectoryAccountLogon",
	10:  "DataCenterSecurityCmdlet",
	11:  "ComplianceDLPSharePoint",
	12:  "Sway",
	13:  "ComplianceDLPExchange",
	14:  "SharePointSharingOperation",
	15:  "AzureActiveDirectoryStsLogon",
	16:  "SkypeForBusinessPSTNUsage",
	17:  "SkypeForBusinessUsersBlocked",
	18:  "SecurityComplianceCenterEOPCmdlet",
	19:  "ExchangeAggregatedOperation",
	20:  "PowerBIAudit",
	21:  "CRM",
	22:  "Yammer",
	23:  "SkypeForBusinessCmdlets",
	24:  "Discovery",
	25:  "MicrosoftTeams",
	26:  "MicrosoftTeamsAddOns",
	27:  "MicrosoftStream",
	28:  "ComplianceDLPSharePointClassification",
	29:  "ThreatFinder",
	30:  "Project",
	31:  "SharePointListOperation",
	32:  "SharePointCommentOperation",
	33:  "DataGovernance",
	34:  "Kaizala",
	35:  "SecurityComplianceAlerts",
	36:  "ThreatIntelligenceUrl",
	37:  "SecurityComplianceInsights",
	38:  "MIPLabel",
	39:  "WorkplaceAnalytics",
	40:  "PowerAppsApp",
	41:  "PowerAppsPlan",
	42:  "ThreatIntelligenceAtpContent",
	43:  "LabelContentExplorer",
	44:  "TeamsHealthcare",
	45:  "ExchangeItemAggregated",
	46:  "HygieneEvent",
	47:  "DataInsightsRestApiAudit",
	48:  "InformationBarrierPolicyApplication",
	49:  "SharePointListItemOperation",
	50:  "SharePointContentTypeOperation",
	51:  "SharePointFieldOperation",
	52:  "MicrosoftTeamsAdmin",
	53:  "HRSignal",
	54:  "MicrosoftTeamsDevice",
	55:  "MicrosoftTeamsAnalytics",
	56:  "InformationWorkerProtection",
	57:  "Campaign",
	58:  "DLPEndpoint",
	59:  "AirInvestigation",
	60:  "Quarantine",
	61:  "MicrosoftForms",
	62:  "ApplicationAudit",
	63:  "ComplianceSupervisionExchange",
	64:  "CustomerKeyServiceEncryption",
	65:  "OfficeNative",
	66:  "MipAutoLabelSharePointItem",
	67:  "MipAutoLabelSharePointPolicyLocation",
	68:  "MicrosoftTeamsShifts",
	70:  "MipAutoLabelExchangeItem",
	71:  "CortanaBriefing",
	72:  "Search",
	73:  "WDATPAlerts",
	78:  "MDATPAudit",
	82:  "SensitivityLabelPolicyMatch",
	83:  "SensitivityLabelAction",
	84:  "SensitivityLabeledFileAction",
	85:  "AttackSim",
	86:  "AirManualInvestigation",
	87:  "SecurityComplianceRBAC",
	88:  "UserTraining",
	89:  "AirAdminActionInvestigation",
	90:  "MSTIC",
	91:  "PhysicalBadgingSignal",
	93:  "AipDiscover",
	94:  "AipSensitivityLabelAction",
	95:  "AipProtectionAction",
	96:  "AipFileDeleted",
	97:  "AipHeartBeat",
	98:  "MCASAlerts",
	99:  "OnPremisesFileShareScannerDlp",
	100: "OnPremisesSharePointScannerDlp",
	101: "ExchangeSearch",
	102: "SharePointSearch",
	103: "PrivacyDataMinimization",
	104: "LabelAnalyticsAggregate",
	105: "MyAnalyticsSettings",
	106: "SecurityComplianceUserChange",
	107: "ComplianceDLPExchangeClassification",
	108: "ComplianceDLPEndpoint",
	109: "MipExactDataMatch",
	110: "MSDEResponseActions",
	111: "MSDERolesSettings",
	112: "MS365DCustomDetection",
	113: "MSDEIndicatorsSettings",
	147: "CoreReportingSettings",
	148: "ComplianceConnector",
	181: "MicrosoftGraphDataConnectOperation",
	183: "PowerPagesSite",
	186: "PlannerPlan",
	187: "PlannerCopyPlan",
	188: "PlannerTask",
	189: "PlannerRoster",
	216: "MicrosoftTeamsSensitivityLabelAction",
	217: "OneDriveForBusinessSensitivityLabelAction",
}

// userTypes maps the numeric UserType to the symbolic name from the common
// audit schema.
var userTypes = map[int]string{
	0:  "Regular",
	1:  "Reserved",
	2:  "Admin",
	3:  "DcAdmin",
	4:  "System",
	5:  "Application",
	6:  "ServicePrincipal",
	7:  "CustomPolicy",
	8:  "SystemPolicy",
	9:  "PartnerTechnician",
	10: "Guest",
}

// RecordTypeName returns the symbolic name for an audit RecordType.
func RecordTypeName(v int) string {
	if name, ok := recordTypes[v]; ok {
		return name
	}
	return "RecordType_" + strconv.Itoa(v)
}

// UserTypeName returns the symbolic name for an audit UserType.
func UserTypeName(v int) string {
	if name, ok := userTypes[v]; ok {
		return name
	}
	return "UserType_" + strconv.Itoa(v)
}
