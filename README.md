# timeout [中文](./README-cn.md)

Implement function-level timeout control in Golang, minimizing the code into business logic.

原理参考[go 业务超时控制](https://xizi.in/go/timeout.html).


``` go
// @timeout
func GetByID(id int) (string, error) {
    return "foo", nil
}
```

Exec `timeout .` The following code will be generated.
``` go
func GetByIDWithTimeout(id int, timeout time.Duration) (string, error) {
	if timeout == 0 {
		return GetByID(id)
	}

	nctx := context.Background()
	nctx, cancel := context.WithTimeout(nctx, timeout)
	defer cancel()

	resultChan := make(chan error, 1)
	var r0 string
	var r1 error
	go func() {
		r0, r1 = GetByID(id)
		resultChan <- nil
	}()

	select {
	case <-nctx.Done():
		return r0, ErrTimeout
	case _ = <-resultChan:
		return r0, r1
	}

	return r0, r1
}
```

When adjusting the business code, you only need to change `GetByID(1)` to `GetByIDWithTimeout(1, time.Second)`.


## How to use?

Add the comment // @timeout to the functions that require timeout control, and then execute.

`timeout .`

## How to install?

``` bash
go install github.com/crazyhulk/timeout@latest
```
