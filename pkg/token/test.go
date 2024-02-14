package token

import (
	"crypto/rand"
	"fmt"
	_ "net/http/pprof"
	"time"

	"github.com/AthenZ/k8s-athenz-sia/v3/third_party/log"
)

func testing(d *daemon) {
	go func() {

		time.Sleep(30 * time.Second)
		addDummy := func(startIndex, count int) {
			log.Warn("🌟Generating fake tokens for testing ...")
			for i := startIndex; i < startIndex+count; i++ {
				// generate random token
				key := CacheKey{
					Domain: fmt.Sprintf("domain%d", i),
					Role:   fmt.Sprintf("role%d", i),
				}
				atBuf := make([]byte, 750) // average access token size
				rand.Read(atBuf)
				rtBuf := make([]byte, 550) // average role token size
				rand.Read(rtBuf)

				// store token in cache
				d.accessTokenCache.Store(key, &AccessToken{
					domain: key.Domain,
					role:   key.Role,
					raw:    string(atBuf),
					scope:  "",
					expiry: 1864233600,
				})
				d.roleTokenCache.Store(key, &RoleToken{
					domain: key.Domain,
					role:   key.Role,
					raw:    string(rtBuf),
					expiry: 1864233600,
				})
			}

			log.Warn("🌟Generating fake tokens for testing ...END")
		}

		// 20k each call for each type, to 100k token (total 200k at+rt)
		addDummy(0, 100)
	}()
}
