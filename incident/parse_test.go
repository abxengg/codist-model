package incident

import "testing"

const springLog = `2026-11-05T10:15:02.123Z ERROR 1 --- [order-service] [nio-8081-exec-3] o.a.c.c.C.[.[.[/].[dispatcherServlet] : Servlet.service() for servlet [dispatcherServlet] threw exception
org.springframework.web.util.NestedServletException: Request processing failed: java.lang.NullPointerException: Cannot invoke "com.orbit.order.order.Order.getAmount()" because "o" is null
	at org.springframework.web.servlet.FrameworkServlet.processRequest(FrameworkServlet.java:1014)
	at org.springframework.web.servlet.FrameworkServlet.doGet(FrameworkServlet.java:903)
Caused by: java.lang.NullPointerException: Cannot invoke "com.orbit.order.order.Order.getAmount()" because "o" is null
	at com.orbit.order.order.OrderTotals.totalFor(OrderTotals.java:18)
	at com.orbit.order.order.OrderController.total(OrderController.java:41)
	at java.base/jdk.internal.reflect.DirectMethodHandleAccessor.invoke(DirectMethodHandleAccessor.java:103)
	... 48 common frames omitted
2026-11-05T10:15:09.001Z  WARN 1 --- [order-service] [nio-8081-exec-5] c.o.o.order.OrderService : slow query
2026-11-05T10:16:02.555Z ERROR 1 --- [order-service] [nio-8081-exec-7] o.a.c.c.C.[.[.[/].[dispatcherServlet] : Servlet.service() threw exception
org.springframework.web.util.NestedServletException: Request processing failed: java.lang.NullPointerException: Cannot invoke "com.orbit.order.order.Order.getAmount()" because "o" is null
	at org.springframework.web.servlet.FrameworkServlet.processRequest(FrameworkServlet.java:1014)
Caused by: java.lang.NullPointerException: Cannot invoke "com.orbit.order.order.Order.getAmount()" because "o" is null
	at com.orbit.order.order.OrderTotals.totalFor(OrderTotals.java:18)
	at com.orbit.order.order.OrderController.total(OrderController.java:41)
2026-11-05T10:17:44.010Z ERROR 1 --- [payment-service] [nio-8084-exec-1] c.o.p.PaymentService : charge failed
java.lang.IllegalStateException: Card 4111 declined
	at com.orbit.payment.PaymentService.charge(PaymentService.java:77)
`

func TestParseGroupsAndFindsTheRootCause(t *testing.T) {
	errs := Parse(springLog)
	if len(errs) != 2 {
		t.Fatalf("want 2 distinct errors, got %d: %+v", len(errs), errs)
	}
	npe := errs[0]
	if npe.Count != 2 || npe.Type != "NestedServletException" || len(npe.Causes) != 1 {
		t.Errorf("grouped error: %+v", npe)
	}
	if npe.Root == "" || npe.Root[:20] != "NullPointerException" {
		t.Errorf("root cause: %q", npe.Root)
	}
	if len(npe.Frames) != 3 || npe.Frames[0].Class != "com.orbit.order.order.OrderTotals" || npe.Frames[0].Line != 18 || npe.Frames[0].Method != "totalFor" {
		t.Errorf("frames should be the cause's: %+v", npe.Frames)
	}
	if npe.Frames[2].Class != "jdk.internal.reflect.DirectMethodHandleAccessor" {
		t.Errorf("module prefix not stripped: %+v", npe.Frames[2])
	}
	if errs[1].Type != "IllegalStateException" || errs[1].Message != "Card 4111 declined" || errs[1].Frames[0].Line != 77 {
		t.Errorf("second error: %+v", errs[1])
	}
}

func TestParseIgnoresPlainLines(t *testing.T) {
	if errs := Parse("INFO started\nWARN slow query took 300ms\n"); len(errs) != 0 {
		t.Errorf("no errors expected: %+v", errs)
	}
}

func TestParseKeepsTheOuterFrames(t *testing.T) {
	errs := Parse(springLog)
	if o := errs[0].Outer; len(o) != 2 || o[0].Method != "processRequest" {
		t.Errorf("outer frames: %+v", o)
	}
	if errs[1].Outer != nil {
		t.Errorf("an error with no cause has no separate outer frames: %+v", errs[1].Outer)
	}
}

func TestParseNestedAndUnusualExceptionNames(t *testing.T) {
	log := `2026-11-19T09:14:02.100+05:30 ERROR 1 --- [order-service] [nio-8081-exec-1] o.a.c.c.C.[.[.[/].[dispatcherServlet] : Servlet.service() threw exception
feign.FeignException$NotFound: [404] during [POST] to [http://payment-service/payments/charges]
	at feign.FeignException.clientErrorStatus(FeignException.java:219)
	at com.shoplab.order.OrderService.pay(OrderService.java:28)
2026-11-19T14:02:31.000+05:30 ERROR 1 --- [payment-service] [nio-8082-exec-3] c.s.payment.PaymentService : charge failed
com.shoplab.payment.CardGateway$GatewayTimeout: no answer from acquirer within 8000 ms
	at com.shoplab.payment.PaymentService.charge(PaymentService.java:26)
2026-11-19T14:02:32.000+05:30  INFO 1 --- [payment-service] [main] c.s.payment.Config : using com.shoplab.payment.HttpCardGateway
`
	errs := Parse(log)
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %d: %+v", len(errs), errs)
	}
	if errs[0].Type != "FeignException$NotFound" || errs[0].Frames[1].Line != 28 {
		t.Errorf("feign error: %+v", errs[0])
	}
	if errs[1].Type != "CardGateway$GatewayTimeout" || errs[1].Frames[0].Method != "charge" {
		t.Errorf("gateway error: %+v", errs[1])
	}
}

func TestParseCountsALoggedExceptionAndItsTraceOnce(t *testing.T) {
	log := `2026-11-19T11:05:10.000+05:30 ERROR 1 --- [order-service] [nio-8081-exec-1] o.a.c.c.C.[.[.[/].[dispatcherServlet] : Servlet.service() threw exception [Request processing failed: java.util.NoSuchElementException: No value present] orderId=90123
java.util.NoSuchElementException: No value present
	at java.base/java.util.Optional.get(Optional.java:143)
	at com.shoplab.order.OrderService.customerOf(OrderService.java:32)
`
	errs := Parse(log)
	if len(errs) != 1 || errs[0].Count != 1 || len(errs[0].Frames) != 2 {
		t.Fatalf("want one error with its frames, got %+v", errs)
	}
}
