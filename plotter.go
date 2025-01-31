package main

import (
	"fmt"
	"github.com/prometheus/prometheus/promql"
	"image/color"
	"io"
	"log"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/palette/brewer"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
)

// Only show important part of metric name
var labelText = regexp.MustCompile("{(.*)}")

func GetPlotExpr(alertFormula string) []PlotExpr {
	expr, _ := promql.ParseExpr(alertFormula)
	if parenExpr, ok := expr.(*promql.ParenExpr); ok {
		expr = parenExpr.Expr
		log.Printf("Removing redundant brackets: %v", expr.String())
	}

	if binaryExpr, ok := expr.(*promql.BinaryExpr); ok {
		var alertOperator string

		switch binaryExpr.Op {
		case promql.ItemLAND:
			log.Printf("Logical condition, drawing sides separately")
			return append(GetPlotExpr(binaryExpr.LHS.String()), GetPlotExpr(binaryExpr.RHS.String())...)
		case promql.ItemLTE, promql.ItemLSS:
			alertOperator = "<"
		case promql.ItemGTE, promql.ItemGTR:
			alertOperator = ">"
		default:
			log.Printf("Unexpected operator: %v", binaryExpr.Op.String())
			alertOperator = ">"
		}

		alertLevel, _ := strconv.ParseFloat(binaryExpr.RHS.String(), 64)
		return []PlotExpr{PlotExpr{
			Formula:  binaryExpr.LHS.String(),
			Operator: alertOperator,
			Level:    alertLevel,
		}}
	} else {
		log.Printf("Non binary excpression: %v", alertFormula)
		return nil
	}
}

func Plot(expr PlotExpr, queryTime time.Time, duration, resolution time.Duration, prometheusUrl string, alert Alert) io.WriterTo {
	log.Printf("Querying Prometheus %s", expr.Formula)
	metrics, err := Metrics(
		prometheusUrl,
		expr.Formula,
		queryTime,
		duration,
		resolution,
	)
	fatal(err, "failed to get metrics")

	var selectedMetrics model.Matrix
	var founded bool
	for _, metric := range metrics {
		log.Printf("Metric fetched: %v", metric.Metric)
		founded = false
		for label, value := range metric.Metric {
			if originValue, ok := alert.Labels[string(label)]; ok {
				if originValue == string(value) {
					founded = true
				} else {
					founded = false
					break
				}
			}
		}

		if founded {
			log.Printf("Best match founded: %v", metric.Metric)
			selectedMetrics = model.Matrix{metric}
			break
		}
	}

	if !founded {
		log.Printf("Best match not founded, use entire dataset. Labels to search: %v", alert.Labels)
		selectedMetrics = metrics
	}

	log.Printf("Creating plot: %s", alert.Annotations["summary"])
	plottedMetric, err := PlotMetric(selectedMetrics, expr.Level, expr.Operator)
	fatal(err, "failed to create plot")

	return plottedMetric
}

func PlotMetric(metrics model.Matrix, level float64, direction string) (io.WriterTo, error) {
	p, err := plot.New()
	if err != nil {
		return nil, fmt.Errorf("failed to create new plot: %v", err)
	}

	textFont, err := vg.MakeFont("Helvetica", 3*vg.Millimeter)
	if err != nil {
		return nil, fmt.Errorf("failed to load font: %v", err)
	}

	evalTextFont, err := vg.MakeFont("Helvetica", 5*vg.Millimeter)
	if err != nil {
		return nil, fmt.Errorf("failed to load font: %v", err)
	}

	evalTextStyle := draw.TextStyle{
		Color:  color.NRGBA{A: 150},
		Font:   evalTextFont,
		XAlign: draw.XRight,
		YAlign: draw.YBottom,
	}

	p.X.Tick.Marker = plot.TimeTicks{Format: "15:04:05"}
	p.X.Tick.Label.Font = textFont
	p.Y.Tick.Label.Font = textFont
	p.Legend.Font = textFont
	p.Legend.Top = true
	p.Legend.YOffs = 15 * vg.Millimeter

	// Color palette for drawing lines
	paletteSize := 8
	palette, err := brewer.GetPalette(brewer.TypeAny, "Dark2", paletteSize)
	if err != nil {
		return nil, fmt.Errorf("failed to get color palette: %v", err)
	}
	colors := palette.Colors()

	var lastEvalValue float64

	for s, sample := range metrics {
		data := make(plotter.XYs, len(sample.Values))
		for i, v := range sample.Values {
			data[i].X = float64(v.Timestamp.Unix())
			f, err := strconv.ParseFloat(v.Value.String(), 64)
			if err != nil {
				return nil, fmt.Errorf("sample value not float: %s", v.Value.String())
			}
			data[i].Y = f
			lastEvalValue = f
		}

		l, err := plotter.NewLine(data)
		if err != nil {
			return nil, fmt.Errorf("failed to create line: %v", err)
		}
		l.LineStyle.Width = vg.Points(1)
		l.LineStyle.Color = colors[s%paletteSize]

		p.Add(l)
		if len(metrics) > 1 {
			m := labelText.FindStringSubmatch(sample.Metric.String())
			if m != nil {
				p.Legend.Add(m[1], l)
			}
		}
	}

	var polygonPoints plotter.XYs

	if direction == "<" {
		polygonPoints = plotter.XYs{{X: p.X.Min, Y: level}, {X: p.X.Max, Y: level}, {X: p.X.Max, Y: p.Y.Min}, {X: p.X.Min, Y: p.Y.Min}}
	} else {
		polygonPoints = plotter.XYs{{X: p.X.Min, Y: level}, {X: p.X.Max, Y: level}, {X: p.X.Max, Y: p.Y.Max}, {X: p.X.Min, Y: p.Y.Max}}
	}

	poly, err := plotter.NewPolygon(polygonPoints)
	if err != nil {
		log.Panic(err)
	}
	poly.Color = color.NRGBA{R: 255, A: 40}
	poly.LineStyle.Color = color.NRGBA{R: 0, A: 0}
	p.Add(poly)
	p.Add(plotter.NewGrid())

	// Draw plot in canvas with margin
	margin := 6 * vg.Millimeter
	width := 20 * vg.Centimeter
	height := 10 * vg.Centimeter
	c, err := draw.NewFormattedCanvas(width, height, "png")
	if err != nil {
		return nil, fmt.Errorf("failed to create canvas: %v", err)
	}

	cropedCanvas := draw.Crop(draw.New(c), margin, -margin, margin, -margin)
	p.Draw(cropedCanvas)

	// Draw last evaluated value
	evalText := fmt.Sprintf("latest evaluation: %.2f", lastEvalValue)

	plotterCanvas := p.DataCanvas(cropedCanvas)

	trX, trY := p.Transforms(&plotterCanvas)
	evalRectangle := evalTextStyle.Rectangle(evalText)

	points := []vg.Point{
		{X: trX(p.X.Max) + evalRectangle.Min.X - 8*vg.Millimeter, Y: trY(lastEvalValue) + evalRectangle.Min.Y - vg.Millimeter},
		{X: trX(p.X.Max) + evalRectangle.Min.X - 8*vg.Millimeter, Y: trY(lastEvalValue) + evalRectangle.Max.Y + vg.Millimeter},
		{X: trX(p.X.Max) + evalRectangle.Max.X - 6*vg.Millimeter, Y: trY(lastEvalValue) + evalRectangle.Max.Y + vg.Millimeter},
		{X: trX(p.X.Max) + evalRectangle.Max.X - 6*vg.Millimeter, Y: trY(lastEvalValue) + evalRectangle.Min.Y - vg.Millimeter},
	}
	plotterCanvas.FillPolygon(color.NRGBA{R: 255, G: 255, B: 255, A: 90}, points)
	plotterCanvas.FillText(evalTextStyle, vg.Point{X: trX(p.X.Max) - 6*vg.Millimeter, Y: trY(lastEvalValue)}, evalText)

	return c, nil
}
