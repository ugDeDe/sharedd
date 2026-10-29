package cryptoutil

// AES-GCM секреты Beget-интеграции (docs/specs/beget-provisioning.md §2).
// master.key — 32B в отдельном файле ([database] master_key_path, 0600),
// НЕ в БД. Шифруются: login/password Beget и приватный SSH-ключ.
// Портировано из mtplane/internal/cryptoutil с упрощением: путь к ключу
// задаётся явно (файл вне БД), самодельная маршалка OpenSSH заменена на
// штатный golang.org/x/crypto/ssh.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// LoadMasterKey читает master-ключ из отдельного файла. Файл отсутствует →
// создаётся (0600). Существующий, но нечитаемый/битый — фатальная ошибка:
// автоматическая перезапись уничтожила бы возможность расшифровать уже
// сохранённые секреты.
func LoadMasterKey(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("cryptoutil: empty master key path")
	}
	b, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		// Уже существующий ключ: групповые/прочие биты доступа = утечка
		// (M4). Отказываем громко, не принимая скомпрометированный ключ.
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("master.key %s имеет небезопасные права %04o (нужно 0600): файл читаем группой/другими", path, info.Mode().Perm())
		}
		if raw, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(raw) == 32 {
			return raw, nil
		}
		return nil, fmt.Errorf("master.key %s повреждён: нужны 64 hex-символа (32B)", path)
	case !os.IsNotExist(readErr):
		return nil, fmt.Errorf("master.key %s не читается: %w", path, readErr)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(raw)), 0o600); err != nil {
		return nil, fmt.Errorf("master.key %s не создаётся: %w", path, err)
	}
	return raw, nil
}

// Seal шифрует строку (пустая строка → пустая: нечего хранить).
func Seal(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Open расшифровывает строку из Seal. Ошибка — только при неверном ключе/порче.
func Open(key []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// GenerateSSHKeyPair — ed25519-пара в форматах authorized_keys и OpenSSH
// private key (приватный ключ затем шифруется Seal и в открытом виде не хранится).
func GenerateSSHKeyPair() (pub, priv string, err error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	pubKey, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pubKey))), string(pem.EncodeToMemory(block)), nil
}
