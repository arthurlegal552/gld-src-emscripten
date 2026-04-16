# Deploy Público WebXash - Guia Completo

---

## 🚨 Motivos pelos quais NÃO funcionava na internet

| Problema | Explicação |
|---|---|
| 1. **Sem WSS (WebSocket Seguro)** | Navegadores bloqueiam conexões `ws://` em páginas HTTPS. O servidor original só suporta WS plano. |
| 2. **CORS Aberto Universal** | `*` funciona em localhost mas é bloqueado por proxies e WAFs em produção. |
| 3. **Sem Proxy Reverso TLS** | Servidores Go não devem expor TLS diretamente na internet - precisa de Nginx/Caddy na frente. |
| 4. **Servidor TURN Público** | `openrelay.metered.ca` tem limite de banda e bloqueia conexões massivas. |
| 5. **Sem Limite de Conexões** | Ataques ou picos de tráfego derrubam o servidor sem controle. |
| 6. **Timeout Padrão Muito Baixo** | Handshake WebSocket falha em conexões com latência alta. |
| 7. **Headers Não Propagados** | Proxies removem cabeçalhos necessários para WebSocket. |

---

## ✅ Arquitetura Final Recomendada

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Cliente Web    │────▶│  Proxy Reverso  │────▶│  Servidor Xash  │
│  (Netlify)      │ WSS │  (Caddy/Nginx)  │ WS  │  (Go + SFU)     │
│  HTTPS          │     │  TLS Automático │     │  0.0.0.0:27016  │
└─────────────────┘     └─────────────────┘     └─────────────────┘
                                 │
                                 ▼
                        ┌─────────────────┐
                        │  Servidor TURN  │
                        │  (Coturn)       │
                        └─────────────────┘
```

---

## 🚀 Passo a Passo Deploy Funcional

### 1. Deploy no Render (Mais Fácil)

1. Crie um novo Web Service no Render
2. Conecte seu repositório
3. Configurações:
   - Build Command: `cd examples/webrtc-hl && go build -o server .`
   - Start Command: `./examples/webrtc-hl/server`
   - Porta: `27016`
4. Adicione estas variáveis de ambiente:
   ```
   PORT=10000
   MAX_CONNECTIONS=16
   ALLOWED_ORIGINS=https://seu-cliente.netlify.app
   ```

✅ Render fornece WSS automaticamente! Você não precisa configurar TLS.

### 2. Deploy em VPS (Ubuntu/Debian)

```bash
# Instale Docker e Docker Compose
sudo apt update && sudo apt install docker.io docker-compose-plugin

# Clone o repositório
git clone https://github.com/seu-usuario/webxash.git
cd webxash/examples/webrtc-hl

# Edite Caddyfile com seu domínio
nano Caddyfile

# Edite docker-compose.prod.yml com seus domínios
nano docker-compose.prod.yml

# Inicie tudo
docker compose -f docker-compose.prod.yml up -d
```

### 3. Configuração do Cliente

No seu frontend, altere a conexão para:
```javascript
// PRODUÇÃO
const ws = new WebSocket('wss://seu-dominio.com/websocket');

// OU para Render
const ws = new WebSocket('wss://seu-app.onrender.com/websocket');
```

---

## ✅ Checklist Pré-Deploy

- [ ] `ALLOWED_ORIGINS` configurado com domínio real do cliente
- [ ] Servidor escutando em `0.0.0.0` (já está correto)
- [ ] Portas TCP 27016 e UDP 27015 abertas no firewall
- [ ] Cliente usando `wss://` e não `ws://`
- [ ] Sem `localhost` ou `127.0.0.1` em lugar nenhum
- [ ] `MAX_CONNECTIONS` definido (padrão 32)
- [ ] Proxy reverso com header `Upgrade` e `Connection` habilitados
- [ ] Servidor STUN/TURN acessível publicamente

---

## 🛠️ Comandos Úteis

```bash
# Ver logs do servidor
docker compose logs -f webxash-server

# Ver logs do Caddy
docker compose logs -f caddy

# Reiniciar servidor
docker compose restart webxash-server

# Testar conexão WebSocket
wscat -c wss://seu-dominio.com/websocket
```

---

## ❌ Problemas Comuns e Soluções

| Sintoma | Solução |
|---|---|
| WebSocket fica `pending` | Você está usando `ws://` em página HTTPS. Mude para `wss://` |
| `403 Forbidden` no handshake | Origem não está na lista `ALLOWED_ORIGINS` |
| ICE falha / não conecta | Adicione um servidor TURN próprio (coturn) |
| Lag alto / pacotes perdidos | Aumente buffer no WebSocket ou use servidor mais próximo |
| Servidor cai com muitos usuários | Diminua `MAX_CONNECTIONS` ou use VPS com mais CPU |

---

## 🔒 Segurança em Produção

1. **NUNCA** deixe `ALLOWED_ORIGINS=*` em produção
2. Use servidor TURN próprio ao invés do público
3. Adicione autenticação básica no handshake WebSocket
4. Limite taxa de conexões com fail2ban
5. Sempre atualize as dependências do Go
