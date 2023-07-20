# Cookie flipper service

The cookie flipper is meant to be an example service for the metal-toolbox ecosystem
of services in which a [controller](https://github.com/metal-toolbox/architecture/blob/main/firmware-install-service.md#controllers) full-fills a  `cookieFlip` [Condition](https://github.com/metal-toolbox/architecture/blob/main/firmware-install-service.md#conditions) as they are queued up on the NATs Jetstream,
the `cookieFlip` condition may be initiated(queued) by either the [conditionorc API](https://github.com/metal-toolbox/conditionorc) or by another service.

The cookie flipper service listens for events that are published with the `cookieFlip` subject suffix,
it then unpacks the [Condition](https://github.com/metal-toolbox/conditionorc/blob/main/pkg/types/types.go#L104) within the event and proceeds to full-fill the condition based on the [parameters](pkg/types/types.go) included.

The `cookieFlip` condition is full-filled by the flipper by retrieving the cookie based on its identifier
from the store, and then performing flips and delays based on the [parameters](pkg/types/types.go) specified.

As the cookie is being flipped, the flipper publishes its progress to the NATS k/v store.


## Queuing a `cookieFlip` condition

An end user can request a `cookieFlip` condition by calling out to the [conditionorc API]() service,

```bash
curl -Lv -X POST -d '{
            "exclusive": true,
            "parameters":
                {
                    "cookieID": "ede81024-f62a-4288-8730-3fab8cceab78",
                    "flips": 200,
                    "flip_delay": 20s
                }
            }' \

        localhost:9001/api/v1/servers/ede81024-f62a-4288-8730-3fab8cceab78/condition/cookieFlip
```

The status can then be checked using
```bash
    curl -Lv \
     localhost:9001/api/v1/servers/ed33d13d-bb56-42d8-aff4-594d4acbcbbf/condition/cookieFlip
```
