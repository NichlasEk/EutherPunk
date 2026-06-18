# EutherPunk Plan

Datum: 2026-06-18

## Målbild

EutherPunk ska bli en lokal AI-agent med cyberpunk-känsla, men med praktisk nytta först: chatta, hjälpa till att konfigurera miljöer, läsa och skriva kod i kontrollerade arbetsytor, felsöka projekt och på sikt prata via mobilapp med både text och TTS.

Den ska gå att nå via:

- EutherOxide på LAN: `http://192.168.32.186:8080`
- EutherOxide online: `https://apothictech.se`
- SSH med nyckel via befintligt serverflöde
- Nedladdningsbart CLI för Windows, macOS, Linux och Android/Termux
- Senare en mobilapp med textchatt och röstläge

## Utgångsläge

Lokalt finns redan en LLM-miljö under:

```text
/home/nichlas/ai/llm
```

Den dokumenterar Ollama som primär runtime eftersom lokal `llama-server` tidigare inte hittade användbara GPU-enheter.

Första rimliga modell:

```text
qwen3-coder:30b
```

Det är en bra start för kod, konfiguration och agentarbete. Den bör inte hårdkodas som slutgiltig identitet för EutherPunk. Planen bör göra modellvalet konfigurerbart från början så vi kan byta mellan Qwen, framtida lokala modeller och eventuella mindre mobila modeller.

## Rekommenderad Arkitektur

EutherPunk bör delas i fyra lager.

1. Runtime

Kör själva modellen lokalt. Första runtime blir Ollama på `127.0.0.1:11434` eller bakom en intern service på servern. Direkt exponering mot internet ska undvikas.

2. Agent API

Egen EutherPunk-service som pratar med Ollama och lägger på:

- systemprompt och personlighet
- sessionshistorik
- tool-policy
- fil- och repo-behörigheter
- audit-logg
- streaming-svar
- användarprofiler och projektprofiler

3. Gateway via EutherOxide

EutherOxide ska vara den yta användaren möter. Den bör proxy:a till EutherPunk internt och hantera LAN/WAN-routing, auth, nedladdningar och status.

4. Klienter

CLI, webbvy och mobilapp ska prata med samma EutherPunk API. Då slipper varje klient implementera egen modell-logik.

Webblagret bör vara en thin client: browsern hanterar snabb UI, lokal TTS och voice input när det stöds, medan EutherPunk API håller modellval, session, auth och tool-policy. Server-side TTS/STT kan läggas bakom samma API senare när browserstöd inte räcker.

## API-Skiss

Första interna endpoints:

```text
GET  /api/eutherpunk/status
GET  /api/eutherpunk/models
GET  /api/eutherpunk/users
POST /api/eutherpunk/chat
POST /api/eutherpunk/chat/stream
POST /api/eutherpunk/sessions
GET  /api/eutherpunk/sessions/{id}
POST /api/eutherpunk/tools/plan
POST /api/eutherpunk/tools/apply
GET  /downloads/eutherpunk-cli/{platform}
```

`/tools/plan` och `/tools/apply` bör separeras. Agenten får gärna föreslå ändringar, men verkliga filändringar och kommandon ska ligga bakom tydliga policies och helst explicit godkännande i början.

## CLI

CLI:t bör vara den första riktiga klienten eftersom det ger nytta snabbt.

Förslag:

- Rust eller Go för en enda binär per plattform
- config i TOML under användarens hemkatalog
- stöd för LAN/WAN auto-discovery
- interaktiv chat
- `eutherpunk ask`
- `eutherpunk doctor`
- `eutherpunk login`
- `eutherpunk project init`
- `eutherpunk code review`
- `eutherpunk patch --dry-run`

För Android bör Termux vara första mål. En native Android-app kommer senare.

Exempel på config:

```toml
profile = "default"

[server]
lan_url = "http://192.168.32.186:8080"
public_url = "https://apothictech.se"
prefer_lan = true

[agent]
model = "qwen3-coder:30b"
safe_mode = true
```

## Mobilapp

Mobilappen bör vänta tills API och CLI är stabila.

Första mobilversion:

- textchatt
- push-to-talk
- TTS-svar
- sessionhistorik
- modell/statusvy
- enkel filöverföring senare

TTS bör återanvända befintliga EutherLink/EutherBooks-erfarenheter i stället för att byggas från noll. Dots/VoxCPM2-spåret finns redan i `/home/nichlas/ai` och bör ses som kandidat för röstläge.

Första webbröstläget kan börja tunnare:

- `speechSynthesis` i browsern för TTS
- `POST /api/eutherpunk/tts` för serverröst via EutherLink GrapheneOS Matcha
- `SpeechRecognition` i browsern för voice-to-text där det stöds
- server-side STT/TTS senare när vi vill ha samma kvalitet på alla klienter

## Säkerhetsmodell

Det här är viktigaste designfrågan.

EutherPunk ska kunna hjälpa till med kod och konfigurering, men fjärrkörning kan bli farlig om den byggs för öppet.

Grundregler:

- Ollama ska inte exponeras direkt mot internet.
- EutherOxide/EutherPunk-gateway ska kräva auth för allt som inte är status eller nedladdning.
- Tool-anrop ska vara allowlistade.
- Skrivande filoperationer ska begränsas till valda arbetsytor.
- Destruktiva kommandon ska kräva explicit godkännande.
- Allt agenten gör ska loggas med session, användare, kommando och arbetskatalog.
- CLI:t ska ha `--dry-run` som default för patchar tills vi litar på flödet.

## Föreslagen Repo-Struktur

```text
EutherPunk/
  docs/
    EUTHERPUNK_PLAN.md
  services/
    eutherpunk-api/
  clients/
    cli/
    mobile/
  config/
    eutherpunk.example.toml
  scripts/
    dev/
  packaging/
    downloads/
```

## Första Milstolpar

### Milstolpe 1: Projektgrund

- Initiera repo och GitHub-remote.
- Lägg in plan och README.
- Skapa config-exempel.
- Bestäm språk för API och CLI.
- Dokumentera hur lokal Ollama/Qwen används.

### Milstolpe 2: Minimal Agent API

- Bygg en liten service som pratar med Ollama.
- Lägg till `/status`, `/models` och `/chat`.
- Stöd streaming.
- Lägg systemprompt/personlighet i config.
- Kör bara lokalt först.

### Milstolpe 3: EutherOxide-koppling

- Lägg EutherPunk routes i serverns EutherOxide-checkout på `192.168.32.186`.
- Proxy:a till intern EutherPunk-service.
- Lägg nedladdningsruta för CLI.
- Verifiera LAN via `192.168.32.186:8080`.
- Verifiera publik väg via `apothictech.se`.

### Milstolpe 4: CLI

- Skapa `eutherpunk` CLI.
- Implementera login/config.
- Implementera `ask`, `chat`, `doctor`.
- Paketera för Linux först.
- Därefter Windows, macOS och Android/Termux.

### Milstolpe 5: Kodagent

- Lägg till projektindexering.
- Implementera repo-läsning.
- Implementera patchförslag.
- Lägg till explicit `apply`.
- Lägg audit-logg och begränsade arbetsytor.

### Milstolpe 6: Mobilapp och Röst

- Bygg enkel chattapp.
- Koppla TTS.
- Lägg push-to-talk.
- Lägg sessionhistorik.
- Utvärdera om röst ska köras via EutherLink, separat EutherPunk endpoint eller EutherOxide gateway.

## Diskussionspunkter

Det här bör vi bestämma innan större implementation:

- Ska EutherPunk API byggas i Rust, Go, Python/FastAPI eller integreras direkt i befintlig Rust-baserad EutherOxide-server?
- Ska CLI:t vara Rust för enkel distribution, eller Go för snabbare initial utveckling?
- Ska första auth-läget vara SSH-tunnel, EutherOxide-login eller separat API-token?
- Hur mycket autonomi ska kodagenten få i första versionen?
- Ska modellen/personligheten heta EutherPunk även om underliggande runtime är Qwen, eller ska EutherPunk vara agentlagret ovanpå flera modeller?
- Ska TTS återanvända EutherLink/EutherBooks direkt eller få ett eget EutherPunk-röstlager?

## Min Rekommendation

Bygg EutherPunk som agentlagret, inte som en hårdkodad modell. Låt första modellen vara `qwen3-coder:30b` via Ollama, men gör runtime konfigurerbar.

Första implementationen bör vara:

1. Rust eller Go CLI.
2. Liten lokal EutherPunk API-service.
3. EutherOxide proxy och nedladdningssida.
4. Kodagent-funktioner bakom tydliga godkännanden.
5. Mobilapp först när API/CLI känns stabila.

Det ger snabb nytta utan att låsa fast oss i fel modell, fel klient eller för öppen fjärrstyrning.
