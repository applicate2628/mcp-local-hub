package scheduler

import (
	"errors"
	"strings"
	"testing"
)

const taskDefinitionFixture = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>autostart</Description><Author>S-1-5-21-1</Author><Date>2026-01-01T12:00:00</Date></RegistrationInfo>
  <Triggers><LogonTrigger><UserId>S-1-5-21-1</UserId><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>S-1-5-21-1</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable><MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle><WakeToRun>false</WakeToRun><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Priority>7</Priority>
  </Settings>
  <Actions Context="Author"><Exec><Command>D:\hub\mcphub.exe</Command><Arguments>gui --no-browser</Arguments></Exec></Actions>
</Task>`

func TestTaskDefinitionsEquivalentAllowsNativeNormalization(t *testing.T) {
	// Microsoft defaults are documented beside taskXMLDefaults. Native exports
	// can omit true Enabled/AllowStartOnDemand and false WakeToRun, or materialize
	// false UseUnifiedSchedulingEngine/Volatile and PT10M/PT1H idle defaults.
	defaults := strings.NewReplacer(
		"<Enabled>true</Enabled>", "",
		"<AllowStartOnDemand>true</AllowStartOnDemand>", "",
		"<WakeToRun>false</WakeToRun>", "",
		"<RunLevel>LeastPrivilege</RunLevel>", "",
		"<Settings>", "<Settings><UseUnifiedSchedulingEngine>false</UseUnifiedSchedulingEngine><Volatile>false</Volatile>",
		"<IdleSettings>", "<IdleSettings><Duration>PT10M</Duration><WaitTimeout>PT1H</WaitTimeout>",
		"<LogonTrigger>", "<LogonTrigger><Delay>PT0M</Delay><ExecutionTimeLimit>PT72H</ExecutionTimeLimit>",
	).Replace(taskDefinitionFixture)
	for _, tc := range []struct {
		name string
		xml  string
	}{
		{name: "identical", xml: taskDefinitionFixture},
		{name: "native_defaults", xml: defaults},
		{name: "boolean_spelling", xml: strings.NewReplacer(">true<", ">1<", ">false<", ">0<").Replace(taskDefinitionFixture)},
		{name: "registration_metadata", xml: strings.NewReplacer(
			"<Description>autostart</Description>", "<Description>operator annotation</Description><URI>\\mcp-local-hub-supervisor</URI>",
			"2026-01-01T12:00:00", "2026-12-01T00:00:00",
		).Replace(taskDefinitionFixture)},
		{name: "unordered_settings_and_path_case", xml: strings.NewReplacer(
			"<Enabled>true</Enabled><Hidden>false</Hidden>", "<Hidden>false</Hidden><Enabled>true</Enabled>",
			`D:\hub\mcphub.exe`, `d:\HUB\MCPHUB.EXE`,
		).Replace(taskDefinitionFixture)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, pair := range [][2]string{{tc.xml, taskDefinitionFixture}, {taskDefinitionFixture, tc.xml}} {
				match, err := taskDefinitionsEquivalent([]byte(pair[0]), []byte(pair[1]))
				if err != nil || !match {
					t.Fatalf("equivalent readback rejected: match=%v err=%v", match, err)
				}
			}
		})
	}
}

func TestTaskDefinitionsEquivalentPreservesBehaviorDifferences(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
	}{
		{name: "command", before: `D:\hub\mcphub.exe`, after: `D:\other\mcphub.exe`},
		{name: "strict_mode", before: "gui --no-browser", after: "gui --no-browser --strict-mode"},
		{name: "owner_mode", before: "gui --no-browser", after: "supervise"},
		{name: "working_directory", before: "</Exec>", after: "<WorkingDirectory>D:\\other</WorkingDirectory></Exec>"},
		{name: "extra_action", before: "</Actions>", after: "<Exec><Command>other.exe</Command></Exec></Actions>"},
		{name: "missing_trigger", before: "<LogonTrigger><UserId>S-1-5-21-1</UserId><Enabled>true</Enabled></LogonTrigger>", after: ""},
		{name: "disabled_trigger", before: "<Enabled>true</Enabled></LogonTrigger>", after: "<Enabled>false</Enabled></LogonTrigger>"},
		{name: "extra_trigger", before: "</Triggers>", after: "<BootTrigger/></Triggers>"},
		{name: "trigger_user", before: "<LogonTrigger><UserId>S-1-5-21-1", after: "<LogonTrigger><UserId>S-1-5-21-2"},
		{name: "principal_user", before: `<Principal id="Author"><UserId>S-1-5-21-1`, after: `<Principal id="Author"><UserId>S-1-5-21-2`},
		{name: "principal_logon", before: "InteractiveToken", after: "Password"},
		{name: "principal_run_level", before: "LeastPrivilege", after: "HighestAvailable"},
		{name: "disabled_task", before: "<Enabled>true</Enabled><Hidden>false", after: "<Enabled>false</Enabled><Hidden>false"},
		{name: "execution_limit", before: "<ExecutionTimeLimit>PT0S", after: "<ExecutionTimeLimit>PT1H"},
		{name: "instance_policy", before: "StopExisting", after: "IgnoreNew"},
		{name: "restart_policy", before: "</Settings>", after: "<RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings>"},
		{name: "expiry_deletion", before: "</Settings>", after: "<DeleteExpiredTaskAfter>PT0S</DeleteExpiredTaskAfter></Settings>"},
		{name: "unknown_setting", before: "</Settings>", after: "<FutureSetting>false</FutureSetting></Settings>"},
		{name: "unknown_attribute", before: "<Settings>", after: `<Settings future="false">`},
		{name: "unknown_registration_field", before: "</RegistrationInfo>", after: "<FutureField/></RegistrationInfo>"},
		{name: "registration_permissions", before: "</RegistrationInfo>", after: "<SecurityDescriptor>fixture</SecurityDescriptor></RegistrationInfo>"},
		{name: "duplicate_default", before: "<Enabled>true</Enabled><Hidden>false", after: "<Enabled>true</Enabled><Enabled>true</Enabled><Hidden>false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := strings.Replace(taskDefinitionFixture, tc.before, tc.after, 1)
			if observed == taskDefinitionFixture {
				t.Fatal("fixture transformation did not change the definition")
			}
			match, err := taskDefinitionsEquivalent([]byte(observed), []byte(taskDefinitionFixture))
			if err != nil || match {
				t.Fatalf("different definition accepted: match=%v err=%v", match, err)
			}
		})
	}
}

func TestTaskDefinitionsEquivalentRejectsUnreadableXML(t *testing.T) {
	for _, raw := range []string{
		"",
		"<Task><private-canary>",
		strings.Replace(taskDefinitionFixture, taskXMLNamespace, "urn:another-schema", 1),
		taskDefinitionFixture + "<Task/>",
	} {
		match, err := taskDefinitionsEquivalent([]byte(raw), []byte(taskDefinitionFixture))
		if match || !errors.Is(err, ErrTaskCorrupt) || strings.Contains(err.Error(), "private-canary") {
			t.Fatalf("invalid XML outcome: match=%v err=%v", match, err)
		}
	}
}
