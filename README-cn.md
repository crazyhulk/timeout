# timeout

golang 实现函数级的超时控制, 尽量减少业务代码的入侵性.

原理参考[go 业务超时控制](https://xizi.in/go/timeout.html).

``` go
// @timeout
func GetByID(id int) (string, error) {
    return "foo", nil
}
```

执行 timeout . 会生成如下代码:
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

调整业务代码的时候也只需要把 `GetByID(1)` ==> `GetByIDWithTimeout(1, time.Second)`.


## 使用方法

在需要添加超时控制的函数上加上注释 `// @timeout` 然后执行

`timeout .`

## 安装方法

``` bash
go install github.com/crazyhulk/timeout@latest
```
