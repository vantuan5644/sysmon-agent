package main

import "testing"

func TestPrivacyRecordRunning(t *testing.T) {
	const start = 1000
	discord := []string{"NonPackaged", `C:#Users#vantu#AppData#Local#Discord#app-1.0.9262#Discord.exe`}
	whatsapp := []string{"5319275A.WhatsAppDesktop_cv1g1gvanyjgm"}
	unpackaged := func(exe string, created uint64) privacyProcess {
		return privacyProcess{exe: exe, created: created, familyOK: true}
	}
	for _, tc := range []struct {
		name      string
		key       []string
		processes []privacyProcess
		want      bool
	}{
		{"owner running since before the capture", discord, []privacyProcess{unpackaged("Discord.exe", 900)}, true},
		{"owner restarted after the capture started", discord, []privacyProcess{unpackaged("Discord.exe", 1100), unpackaged("Discord.exe", 1101)}, false},
		{"one owner process predates the capture", discord, []privacyProcess{unpackaged("Discord.exe", 1100), unpackaged("Discord.exe", 1000)}, true},
		{"owner no longer running", discord, []privacyProcess{unpackaged("brave.exe", 10)}, false},
		{"image name compared without case", discord, []privacyProcess{unpackaged("DISCORD.EXE", 900)}, true},
		{"owner creation time unreadable", discord, []privacyProcess{{exe: "Discord.exe"}}, true},
		{"image name is only a suffix of another", discord, []privacyProcess{unpackaged("cord.exe", 900)}, false},
		{"image name containing '#'", []string{"NonPackaged", `C:#Tools#C#Shell.exe`}, []privacyProcess{unpackaged("C#Shell.exe", 900)}, true},
		{"unreadable process cannot own an unpackaged record", discord, []privacyProcess{{exe: "System"}}, false},
		{"packaged owner running", whatsapp, []privacyProcess{{exe: "WhatsApp.exe", created: 900, family: whatsapp[0], familyOK: true}}, true},
		{"packaged owner restarted", whatsapp, []privacyProcess{{exe: "WhatsApp.exe", created: 1100, family: whatsapp[0], familyOK: true}}, false},
		{"packaged owner gone", whatsapp, []privacyProcess{unpackaged("WhatsApp.exe", 900)}, false},
		{"process with unreadable package", whatsapp, []privacyProcess{{exe: "System"}}, true},
		{"unknown key shape", []string{"NonPackaged"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privacyRecordRunning(privacyRecord{key: tc.key, start: start}, tc.processes)
			if got != tc.want {
				t.Fatalf("running=%v; want %v", got, tc.want)
			}
		})
	}
}
