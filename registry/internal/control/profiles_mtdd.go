package control

import (
	"errors"
)

// MtddProfileName — профиль установки ноды на прокси mtdd
// (https://github.com/ugDeDe/mtdd) + агент sharedd.
const MtddProfileName = "sharedd-node-mtdd"

// MtddProfileDescription — подпись профиля mtdd.
const MtddProfileDescription = "Установка ноды sharedd на прокси mtdd (mtdd + агент)"

// MtddProfileSteps — mtdd ставится собственным install.sh выбранного канала
// (main|dev) без вопросов: sni/port/ad_tag приходят переменными. Пользователей
// и персональный SNI дальше ведёт агент через API mtdd, поэтому секрет здесь не
// нужен (установщик mtdd создаёт служебного пользователя admin). Агент
// sharedd-node ставится web-установщиком канала sharedd_channel; он сам находит
// /etc/mtdd/config.json. Поддержка mtdd есть в канале dev агента.
const MtddProfileSteps = `# Установка mtdd (канал main|dev) без вопросов.
case {{ mtdd_channel }} in
  main|dev) ;;
  *) echo 'mtdd_channel must be main or dev' >&2; exit 1 ;;
esac
TAG={{ ad_tag }}
if ! printf '%s' "$TAG" | grep -qE '^[0-9a-f]{32}$' || [ "$TAG" = 00000000000000000000000000000000 ]; then
  echo 'ad_tag: нужен ненулевой рекламный тег Telegram (32 hex) — mtdd без него не работает' >&2
  exit 1
fi
curl -fsSLo /root/mtdd-install.sh https://raw.githubusercontent.com/ugDeDe/mtdd/{{ mtdd_channel }}/install.sh
bash /root/mtdd-install.sh --branch {{ mtdd_channel }} --domain {{ sni }} --ad-tag "$TAG" --port {{ port }} --user admin </dev/null
systemctl is-active --quiet mtdd

# Установка агента sharedd выбранного канала (stable|dev) и запуск.
# --name={{ label }}: короткое имя, суффикс -<5hex> добавит агент;
# персональный node_secret — Bearer machine-API.
case {{ sharedd_channel }} in
  stable|dev) ;;
  *) echo 'sharedd_channel must be stable or dev' >&2; exit 1 ;;
esac
curl -fsSL https://raw.githubusercontent.com/ugDeDe/sharedd/main/scripts/install_node_web.sh | sudo bash -s -- --channel={{ sharedd_channel }} --registry={{ registry_url }} --name={{ label }} --registry-token={{ node_secret }}

# Проверка: агент поднялся И зарегистрировался в реестре (как в штатном профиле).
systemctl is-active --quiet sharedd-node-agent
TOKEN={{ node_secret }}
URL={{ registry_url }}/api/node/status
for i in {1..30}; do
  if curl -fsS --max-time 5 -H "Authorization: Bearer $TOKEN" "$URL" >/dev/null 2>&1; then
    exit 0
  fi
  sleep 2
done
echo 'node did not register with the registry' >&2
exit 1
`

// MtddProfileVarsSchema — переменные профиля mtdd. ad_tag обязателен (проверяется
// шагом установки); sharedd_channel по умолчанию dev, пока агент с поддержкой
// mtdd не вышел в stable.
const MtddProfileVarsSchema = `{"vars":[{"name":"ad_tag","description":"Рекламный тег Telegram (32 hex, ненулевой) — обязателен"},{"name":"mtdd_channel","description":"Канал mtdd: main или dev","default":"main"},{"name":"sharedd_channel","description":"Канал агента sharedd: stable или dev (поддержка mtdd — в dev)","default":"dev"},{"name":"port","description":"Порт shared_proxy","default":"443"}]}`

// EnsureMtddProfile создаёт опубликованный профиль mtdd, если его ещё нет.
// Существующий не трогаем (содержимое профиля вправе править админ).
func (s *Store) EnsureMtddProfile() (Profile, error) {
	if p, err := s.ProfileByName(MtddProfileName); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Profile{}, err
	}
	p, err := s.CreateProfile(MtddProfileName, MtddProfileDescription, 0)
	if err != nil {
		if errors.Is(err, ErrDuplicateName) { // гонка двух стартов
			return s.ProfileByName(MtddProfileName)
		}
		return Profile{}, err
	}
	if _, err := s.AddProfileVersion(p.ID, MtddProfileSteps, MtddProfileVarsSchema); err != nil {
		return Profile{}, err
	}
	return s.PublishProfile(p.ID)
}
